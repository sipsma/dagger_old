// Base package for apkdag
package apkdag

import (
	"dagger.io/dagger"
)

// TODO: right now this doesn't update the apk pkg db, so if you run apk info in the container it doesn't show the packages even though the packages are actually installed
#Build: {
	// Alpine version to install.
	version: string | *"3.15.0@sha256:21a3deaa0d32a8057914f36584b5288d2e5ecc984380bc0118285c70fa8c9300"

	// List of packages to install
	packages: [pkgName=string]: {
		// NOTE(samalba, gh issue #1532):
		//   it's not recommended to pin the version as it is already pinned by the major Alpine version
		//   version pinning is for future use (as soon as we support custom repositories like `community`,
		//   `testing` or `edge`)
		version: string | *""
	}

	_base: dagger.#Pull & {
		source: "index.docker.io/alpine:\(version)"
	}

	_apksort: dagger.#ApkDAG & {
		base: _base.output
		pkgNames: [
			for pkgName, _ in packages {
				pkgName
			},
		]
	}

	_fetchBase: dagger.#Exec & {
		input: _base.output
		args: ["sh", "-c", "apk update"] // TODO: need better way of managing this so vertex re-runs if and only if relevant apk remotes change
	}

	_pkgFetches: {
		for pkgName in _apksort.sorted {
			"\(pkgName)": dagger.#Exec & {
				input: _fetchBase.output
				args: ["sh", "-c", "apk fetch -s \(pkgName) | gunzip -c | tar x -C /"]
				// TODO: there are extra metadata files that we should remove after unpacking

				// TODO: there should be a way to get the output of a non-/ mount, then Diff wouldn't be needed here
				// mounts: {
				//   "\(pkgName) apk contents": {
				//     dest: "/output"
				//     type: "fs"
				//     contents: dagger.#Scratch
				//   }
				// }
			}
		}
	}

	_pkgLayers: {
		for pkgName, pkgFetch in _pkgFetches {
			"\(pkgName)": dagger.#Diff & {
				lower: _fetchBase.output
				upper: pkgFetch.output
			}
		}
	}

	_merged: dagger.#Merge & {
		inputs: [
			// TODO: use the alpine base layout package instead of the container image as base
			_base.output,
			for _, pkgLayer in _pkgLayers {
				pkgLayer.output
			},
		]
	}

	output: _merged.output
}
