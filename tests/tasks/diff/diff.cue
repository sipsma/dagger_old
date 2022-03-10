package main

import (
	"dagger.io/dagger"
)

dagger.#Plan & {
	actions: {
		image: dagger.#Pull & {
			source: "alpine:3.15.0@sha256:e7d88de73db3d3fd9b2d63aa7f447a10fd0220b7cbf39803c803f2af9ba256b3"
		}

		exec1: dagger.#Exec & {
			input: image.output
			args: [
				"sh", "-c",
				#"""
					mkdir /dir && echo -n foo > /dir/foo && echo -n removeme > /removeme
					"""#,
			]
		}

		exec2: dagger.#Exec & {
			input: exec1.output
			args: [
				"sh", "-c",
				#"""
					echo -n bar > /dir/bar && rm removeme
					"""#,
			]
		}

		removeme: dagger.#WriteFile & {
			input: dagger.#Scratch
			path: "/removeme"
			contents: "removeme"
		}

		test: {
			diff: dagger.#Diff & {
				lower: image.output
				upper: exec2.output
			}

			verify_diff_foo: dagger.#ReadFile & {
				input: diff.output
				path:  "/dir/foo"
			} & {
				contents: "foo"
			}
			verify_diff_bar: dagger.#ReadFile & {
				input: diff.output
				path:  "/dir/bar"
			} & {
				contents: "bar"
			}

			mergediff: dagger.#Merge & {
				inputs: [
					image.output,
					removeme.output,
					diff.output,
				]
			}
			verify_remove: dagger.#Exec & {
				input: mergediff.output
				args: ["test", "!", "-e", "/removeme"]
			}
		}
	}
}
