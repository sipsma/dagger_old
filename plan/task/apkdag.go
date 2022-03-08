package task

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"cuelang.org/go/cue"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	"github.com/rs/zerolog/log"
	"go.dagger.io/dagger/compiler"
	"go.dagger.io/dagger/plancontext"
	"go.dagger.io/dagger/solver"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

func init() {
	Register("ApkDAG", func() Task { return &apkDAGTask{} })
}

type apkDAGTask struct {
}

func (t apkDAGTask) Run(ctx context.Context, pctx *plancontext.Context, s solver.Solver, v *compiler.Value) (*compiler.Value, error) {
	pkgNameVals, err := v.Lookup("pkgNames").List()
	if err != nil {
		return nil, err
	}
	var pkgNames []string
	for _, val := range pkgNameVals {
		name, err := val.String()
		if err != nil {
			return nil, err
		}
		pkgNames = append(pkgNames, name)
	}

	gw := s.GetOptions().Gateway

	baseFS, err := pctx.FS.FromValue(v.Lookup("base"))
	if err != nil {
		return nil, err
	}
	base, err := baseFS.State()
	if err != nil {
		return nil, err
	}

	// TODO: don't hardcode /sbin, set PATH from image config
	// TODO: upgrade forces the "apk info" output not report multiple possible
	// versions most of the time, but it's not a real solution. What we actually
	// need here is more like a library call that just transforms a list of apk
	// packages plus a list of apk repositories into a topologically sorted list
	// of urls where each package + transitive dependency can be downloaded from.
	updatedBase := base.Run(llb.Args([]string{"/sbin/apk", "upgrade"})).Root()

	baseDef, err := updatedBase.Marshal(ctx, llb.Platform(pctx.Platform.Get()))
	if err != nil {
		return nil, err
	}
	baseResult, err := gw.Solve(ctx, client.SolveRequest{
		Definition: baseDef.ToPB(),
	})
	if err != nil {
		return nil, err
	}

	pbPlatform := pb.PlatformFromSpec(pctx.Platform.Get())
	ctr, err := gw.NewContainer(ctx, client.NewContainerRequest{
		Mounts: []client.Mount{{
			Dest:      "/",
			Ref:       baseResult.Ref,
			Readonly:  true,
			MountType: pb.MountType_BIND,
		}},
		Platform: &pbPlatform,
	})
	if err != nil {
		return nil, err
	}

	// TODO: unless we add a cancel for this context, whenever one of the containers here has
	// an error, things just hang forever, which doesn't seem to be expected. Need to verify
	// and fix upstream in buildkit if true.
	ctx, cancel := context.WithCancel(ctx)

	// TODO: The pid1 of a container has to stick around until we are done with all
	// execs into the container, so having it just sleep for 1 min is essentially a
	// timeout on this function as a whole. That's fine but feels weird, not sure 1 min
	// is a good value either.
	_, err = ctr.Start(ctx, client.StartRequest{
		Args: []string{"/bin/sleep", "60"},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	defer func() {
		cancel()
		ctr.Release(ctx)
	}()

	sorted, err := tsort(ctx, pkgNames, func(ctx context.Context, pkgName string) ([]string, error) {
		const bufSize = 4096 // TODO: made up number
		stdoutBuf := bytes.NewBuffer(make([]byte, 0, bufSize))
		stderrBuf := bytes.NewBuffer(make([]byte, 0, bufSize))

		ctrProc, err := ctr.Start(ctx, client.StartRequest{
			// TODO: retrieve PATH from image config, don't hardcode /sbin
			Args:   []string{"/sbin/apk", "info", "-R", pkgName},
			User:   "root",
			Cwd:    "/",
			Stdout: nopCloser{stdoutBuf},
			Stderr: nopCloser{stderrBuf},
		})
		if err != nil {
			return nil, err
		}

		if err := ctrProc.Wait(); err != nil {
			// TODO:
			log.Ctx(ctx).Debug().Msgf("%s err: %v, stderr:\n %s", pkgName, err, stderrBuf.String())
			return nil, err
		}

		var pkgDeps []string
		for i, line := range strings.Split(stdoutBuf.String(), "\n") {
			if i == 0 {
				continue // skip first line
			}
			// TODO: this parsing is not robust, there can be multiple versions reported by "apk info".
			// Calling "apk upgrade" in the base image fixes this in some cases, but not all, in which
			// case we choose whatever version shows up first... i.e. see "apk info -R so:libudev.so.1"
			if line == "" {
				break
			}
			// TODO: for some reason certain pkg deps are printed with an "=", but apk info fails with
			// that input, have to trim the "=" and after ^^ for example, run "apk info -R so:libncursesw.so.6"
			// and look at the ncurses-terminfo-base dep
			line = strings.Split(line, "=")[0]
			pkgDeps = append(pkgDeps, strings.TrimSpace(line))
		}
		// TODO:
		log.Ctx(ctx).Debug().Msgf("%s deps: %+v", pkgName, pkgDeps)
		return pkgDeps, nil
	})
	if err != nil {
		return nil, err
	}

	output := compiler.NewValue()
	if err := output.FillPath(cue.ParsePath("sorted"), sorted); err != nil {
		return nil, err
	}

	return output, nil
}

// TODO: could turn this into a common util for similar non-apk tasks (i.e. aptdag, yumdag, etc.)
func tsort(ctx context.Context, targetPkgNames []string, getDeps func(ctx context.Context, pkgName string) ([]string, error)) ([]string, error) {
	type pkgVtx struct {
		name    string
		deps    map[*pkgVtx]struct{}
		revdeps map[*pkgVtx]struct{}
		added   bool
	}

	// TODO: comment what's happening here, running each vertex in parallel
	pkgs := make(map[string]*pkgVtx)
	var mu sync.Mutex
	var sf singleflight.Group

	eg, ctx := errgroup.WithContext(ctx)

	var add func(string, string) error
	add = func(pkgName string, rdep string) error {
		mu.Lock()
		vtx, ok := pkgs[pkgName]
		if !ok {
			vtx = &pkgVtx{
				name:    pkgName,
				deps:    make(map[*pkgVtx]struct{}),
				revdeps: make(map[*pkgVtx]struct{}),
			}
			pkgs[pkgName] = vtx
		}
		if rvtx, ok := pkgs[rdep]; ok {
			vtx.revdeps[rvtx] = struct{}{}
			rvtx.deps[vtx] = struct{}{}
		}
		mu.Unlock()

		_, err, _ := sf.Do(pkgName, func() (interface{}, error) {
			if vtx.added {
				return nil, nil
			}
			vtx.added = true
			deps, err := getDeps(ctx, pkgName)
			if err != nil {
				return nil, err
			}

			for _, depName := range deps {
				depName := depName
				eg.Go(func() error {
					return add(depName, pkgName)
				})
			}
			return nil, nil
		})
		return err
	}

	for _, pkgName := range targetPkgNames {
		pkgName := pkgName
		eg.Go(func() error {
			return add(pkgName, "")
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	var sorted []string
	var ready []*pkgVtx
	for _, pkg := range pkgs {
		if len(pkg.deps) == 0 {
			ready = append(ready, pkg)
		}
	}
	for len(ready) > 0 {
		var next []*pkgVtx
		for _, pkg := range ready {
			sorted = append(sorted, pkg.name)
			for dep := range pkg.revdeps {
				delete(dep.deps, pkg)
				if len(dep.deps) == 0 {
					next = append(next, dep)
				}
			}
			delete(pkgs, pkg.name)
		}
		ready = next
	}
	if len(pkgs) > 0 {
		return nil, fmt.Errorf("cycle detected") // TODO: more helpful error
	}
	return sorted, nil
}

type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }
