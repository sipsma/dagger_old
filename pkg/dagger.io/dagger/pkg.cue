package dagger

#ApkDAG: {
	$dagger: task: _name: "ApkDAG"
	base: #FS
	pkgNames: [...string] // TODO: specific type for package name? Also, support for pinned version?
	sorted: [...string]
}

