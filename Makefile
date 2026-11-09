# shardpilot-cli build targets.
#
# `make build` produces a static binary for the host platform in bin/.
# `make build-all` cross-compiles the release matrix into dist/ and
# `make checksums` writes dist/SHA256SUMS. CI runs BOTH on every pull
# request — not only at release time — because a release procedure that has
# never been executed is reviewed rather than tested, and the first time it
# runs should not be the time it matters.

BINARY    := shardpilot
PKG       := ./cmd/shardpilot
VERSION   ?= $(shell git describe --tags --exact-match 2>/dev/null)
# `-X` writes the release tag into the binary. Without it every published
# binary reports the development version through `shardpilot version` and in
# its User-Agent, whatever tag it was cut from.
LDFLAGS   := -s -w $(if $(VERSION),-X github.com/shardpilot/shardpilot-cli/internal/cli.version=$(VERSION))
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build build-all archives checksums release-dryrun fmt fmt-check vet test ci clean
.PHONY: public-surface-check export-dryrun

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

build-all:
	@mkdir -p dist
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=dist/$(BINARY)_$${os}_$${arch}; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out $(PKG); \
	done

# Reproducibility: -trimpath (above) keeps absolute build paths out of the
# binary, and the version is stamped rather than derived at runtime, so two
# builds of one commit produce identical bytes.
# ⚠ LICENSE AND NOTICE TRAVEL IN EVERY ARCHIVE. Apache-2.0 §4 requires a
# recipient of an object-form distribution to receive the licence, and §4(d)
# requires the NOTICE to go with it. A release asset is a standalone
# distribution — the person who downloads it may never see this repository.
#
# ⚠ ARCHIVES, NOT RAW BINARIES. A binary downloaded as an HTTP release asset
# does not keep its executable bit, so "put it on your PATH" leaves a Unix user
# with a file they cannot run. tar preserves the mode; zip is the Windows
# convention. The checksums cover the ARCHIVES, because those are what people
# download.
archives:
	@test -d dist || { echo "archives: run 'make build-all' first" >&2; exit 1; }
	@set -e; cp LICENSE NOTICE dist/; cd dist; rm -f *.tar.gz *.zip; \
		for platform in $(PLATFORMS); do \
			os=$${platform%/*}; arch=$${platform#*/}; \
			bin=$(BINARY)_$${os}_$${arch}; \
			if [ "$$os" = "windows" ]; then \
				cp $$bin.exe $(BINARY).exe; zip -q $$bin.zip $(BINARY).exe LICENSE NOTICE; rm -f $(BINARY).exe; \
			else \
				cp $$bin $(BINARY); chmod +x $(BINARY); tar -czf $$bin.tar.gz $(BINARY) LICENSE NOTICE; rm -f $(BINARY); \
			fi; \
		done
	@rm -f dist/LICENSE dist/NOTICE
	@echo "wrote $$(ls dist/*.tar.gz dist/*.zip 2>/dev/null | wc -l | tr -d ' ') archive(s), each carrying LICENSE and NOTICE"

checksums: archives
	@cd dist && rm -f SHA256SUMS && \
		if command -v sha256sum >/dev/null 2>&1; then sha256sum *.tar.gz *.zip > SHA256SUMS; \
		else shasum -a 256 *.tar.gz *.zip > SHA256SUMS; fi
	@echo "wrote dist/SHA256SUMS"; cat dist/SHA256SUMS

# The whole release packaging path, minus the irreversible part. This is what
# CI runs on a pull request: everything a tag build does except creating the
# tag and the GitHub Release.
# ⚠ ORDER-DEPENDENT, SO IT IS SERIALISED EXPLICITLY. Under `make -j` — or an
# inherited parallel MAKEFLAGS, which is the case nobody remembers — make is
# free to run `build-all` and `checksums` at once, and `checksums` depends on
# `archives`, which reads binaries `build-all` may still be writing. The result
# is a checksum file over a half-written archive, which is worse than a
# failure: it is a WRONG checksum published beside a release. `.NOTPARALLEL`
# constrains this makefile's own targets regardless of the -j flag.
.NOTPARALLEL:

release-dryrun: build-all checksums
	@set -e; missing=0; \
		for platform in $(PLATFORMS); do \
			os=$${platform%/*}; arch=$${platform#*/}; \
			out=dist/$(BINARY)_$${os}_$${arch}; \
			if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
			if [ ! -s "$$out" ]; then echo "release-dryrun: MISSING $$out" >&2; missing=1; fi; \
		done; \
		if [ "$$missing" -ne 0 ]; then echo "release-dryrun: FAILED" >&2; exit 1; fi
	@count=$$(grep -c . dist/SHA256SUMS); \
		expected=$$(echo $(PLATFORMS) | wc -w | tr -d ' '); \
		if [ "$$count" -ne "$$expected" ]; then \
			echo "release-dryrun: SHA256SUMS has $$count line(s), expected $$expected" >&2; exit 1; fi
	@cd dist && if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum --check SHA256SUMS; else shasum -a 256 --check SHA256SUMS; fi
	@echo "release-dryrun: OK — $$(echo $(PLATFORMS) | wc -w | tr -d ' ') binaries + checksums"

GO ?= go

# The fleet runs Go 1.27. That is a project rule — not something this file
# infers — so the formatter here is simply the gofmt beside the `go` on PATH.
# Nothing below derives, pins or downloads a toolchain.
#
# What IS checked is the rule itself, because gofmt's output is not stable across
# releases: 1.27 de-indents the continuation lines of a multi-value `return` whose
# operands are composite literals, which 1.25 indents. A contributor on another
# release therefore rewrites files CI then rejects, and re-running the command
# re-creates the diff — a loop, not a warning, and a silent one unless something
# asks. A few lines of assertion turn it into a message on the first run.
#
# The assertion is made against the `go` BESIDE the resolved gofmt, under
# `GOTOOLCHAIN=local`, not against `$(GO) version`. GOROOT is overridable — by the
# environment and by a hand-edited `go env` file — so a 1.27 `go` can hand back a
# GOROOT belonging to another installation, whose `bin/gofmt` would then format
# everything while `$(GO) version` still says 1.27. Identify the formatter you are
# about to run, not the command you used to find it.
#
# `GOTOOLCHAIN=local` on BOTH commands, and the first one is the load-bearing half:
# a bare `go env GOROOT` performs Go's automatic toolchain selection, which downloads
# a release the module asks for and fails offline when it cannot — so the query meant
# to find the installed formatter would itself reach the network. Pinned to local, the
# rule reads exactly as stated: the `go` you have must BE 1.27.3. On the second, it
# stops `go version` reporting the release it would switch TO, which is the value
# being compared against — the check would confirm itself.
#
# And it identifies the FORMATTER, not the command beside it: `go version <file>`
# reports the release that BUILT that binary, where a bare `go version` reports the
# `go` command's own. A GOROOT holding a 1.27.3 `bin/go` beside a `bin/gofmt` copied
# from another release passes every other check here.
#
# The release string is normalised, not pattern-matched: a GOEXPERIMENT build reports
# `go1.27.3-X:boringcrypto`, and a build tag cannot change gofmt's output, so the
# `-X:` suffix is stripped before the comparison rather than failing a FIPS install.
# ⚠ DERIVED FROM go.mod, which is what the workflows install. Written out
# separately, this was a third spelling of one fact, and the release workflow
# proved the cost: it installed a different version and every tag died in
# `make ci` before publishing an asset.
GO_FORMAT_RELEASE := go$(shell awk '/^go /{print $$2; exit}' go.mod)

define resolve_gofmt
goroot="$$(GOTOOLCHAIN=local $(GO) env GOROOT)" || goroot=; \
if [ -z "$$goroot" ]; then \
	echo "cannot read GOROOT from $(GO)" >&2; \
	exit 1; \
fi; \
gofmt_bin="$$goroot/bin/gofmt"; \
if [ ! -x "$$gofmt_bin" ]; then \
	echo "$$gofmt_bin is not executable" >&2; \
	exit 1; \
fi; \
if [ ! -x "$$goroot/bin/go" ]; then \
	echo "$$goroot has no executable bin/go to identify $$gofmt_bin with" >&2; \
	exit 1; \
fi; \
rootver="$$(GOTOOLCHAIN=local "$$goroot/bin/go" version "$$gofmt_bin")" || rootver=; \
rel="$${rootver##*: }"; rel="$${rel%% *}"; rel="$${rel%%-X:*}"; \
if [ "$$rel" != "$(GO_FORMAT_RELEASE)" ]; then \
	echo "this repository is formatted with $(GO_FORMAT_RELEASE) exactly — the release go.mod names and both workflows install — but $$gofmt_bin was built by $${rel:-an installation that could not be identified}. Install $(GO_FORMAT_RELEASE) and put it on PATH, or point GO= at it." >&2; \
	exit 1; \
fi
endef

# `git ls-files` rather than `find`: it honours .gitignore, and `--others` sees a file
# that has been written but not yet added — the file you just edited. `-z` with
# `xargs -0` because a path containing a space or a newline does not survive word
# splitting, and git C-quotes such paths in its default mode.
#
# Two things about the ENVIRONMENT, both of which decide what this command answers
# about rather than how. `GIT_DIR` and its siblings choose which *repository* is
# listed — inherited from a hook or a repo-aware wrapper, one coincident clean path
# from a foreign index keeps the list non-empty while this checkout goes unchecked —
# so they are unset. And the `GIT_*_PATHSPECS` modes re-interpret every pathspec
# below: `GIT_ICASE_PATHSPECS=1` makes the vendor exclusion swallow `Vendor/sub/x.go`,
# an ordinary package, and the literal/noglob modes stop `*.go` matching at all.
# Pinned to 0 for this command rather than trusted.
#
# The exclusions are the ones the go tool itself applies: what lies BELOW a vendor
# directory (a directory *named* vendor is an ordinary package and stays covered),
# `testdata`, and dot- or underscore-prefixed names.
#
# Four failures are checked rather than assumed, because each renders as "clean":
# a listing that fails halfway is not a short listing, a tracked-but-deleted path is
# not a file, and an EMPTY listing is not an all-clean repository. The fourth is not
# a false clean but a false WRITE: `[ -f ]` follows symlinks, so a tracked `*.go`
# link — or any link in its path — would send `gofmt -w` through to the target,
# rewriting a file that may not even be in this checkout. Reproduced; it is a
# refusal now, naming the component that is a link.
#
# And `[ -f ]` is false for a path that is present but UNEXAMINABLE — under an
# unsearchable directory, under something that is not a directory at all, or not a
# regular file itself — which the skip then turns into a silent pass while the
# repository's other files keep the listing non-empty. Those are refusals; only a
# genuinely absent path (deleted, or never materialised in a sparse checkout) is
# still passed over quietly, which is what the deleted-file case needs.
#
# A file under a NESTED `go.mod` is skipped, not refused: it belongs to another
# module, `./...` stops at the boundary, and that module may pin a different release
# whose canonical formatting is not this one. git lists it because git has no idea
# modules exist. The repository's own root `go.mod` is never seen by this walk, since
# the paths are relative and the walk ends before ".".
define collect_go_files
dir="$$(mktemp -d "$${TMPDIR:-/tmp}/gofmt-list.XXXXXX" 2>/dev/null)" || dir=; \
if [ -z "$$dir" ] || [ ! -d "$$dir" ]; then \
	echo "cannot create a temporary directory for the file listing" >&2; \
	exit 1; \
fi; \
trap 'rm -rf "$$dir"' EXIT; \
trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM; \
list="$$dir/list"; \
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
	GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_GRAFT_FILE GIT_PREFIX \
	GIT_SHALLOW_FILE GIT_IMPLICIT_WORK_TREE GIT_NO_REPLACE_OBJECTS \
	GIT_REPLACE_REF_BASE; \
if ! GIT_LITERAL_PATHSPECS=0 GIT_GLOB_PATHSPECS=0 GIT_NOGLOB_PATHSPECS=0 GIT_ICASE_PATHSPECS=0 git ls-files -z --cached --others --exclude-per-directory=.gitignore '*.go' ':(exclude,glob)**/vendor/*/**' ':(exclude,glob)**/testdata/**' ':(exclude,glob)**/.*/**' ':(exclude,glob)**/_*/**' ':(exclude,glob)**/.*' ':(exclude,glob)**/_*' > "$$dir/raw"; then \
	echo "listing the repository files failed — refusing to format from a partial listing" >&2; \
	exit 1; \
fi; \
if ! xargs -0 sh -c 'for f in "$$@"; do \
		p="$$f"; nested=no; \
		while [ -n "$$p" ]; do \
			if [ -L "$$p" ]; then \
				echo "refusing to format: $$f is a symlink or is reached through one ($$p) — gofmt -w follows it and would rewrite the target, which may be outside this checkout" >&2; \
				exit 1; \
			fi; \
			if [ "$$p" != "$$f" ] && [ -f "$$p/go.mod" ]; then \
				nested=yes; \
				break; \
			fi; \
			if [ "$$p" != "$$f" ] && [ -e "$$p" ] && [ ! -d "$$p" ]; then \
				echo "refusing to format: $$f is under $$p, which exists but is not a directory" >&2; \
				exit 1; \
			fi; \
			if [ -d "$$p" ] && [ ! -x "$$p" ]; then \
				echo "refusing to format: $$f is under $$p, which exists but cannot be searched" >&2; \
				exit 1; \
			fi; \
			case "$$p" in */*) p="$${p%/*}" ;; *) p= ;; esac; \
		done; \
		[ "$$nested" = yes ] && continue; \
		if [ -e "$$f" ] && [ ! -f "$$f" ]; then \
			echo "refusing to format: $$f exists but is not a regular file" >&2; \
			exit 1; \
		fi; \
		[ -f "$$f" ] || continue; \
		printf "%s\0" "$$f" || exit 1; \
	done' sh < "$$dir/raw" > "$$list"; then \
	echo "filtering the file listing failed" >&2; \
	exit 1; \
fi; \
if [ ! -s "$$list" ]; then \
	echo "no Go files found to format — the file listing is broken, refusing to report clean" >&2; \
	exit 1; \
fi
endef


fmt:
	@$(resolve_gofmt); \
	$(collect_go_files); \
	if ! xargs -0 "$$gofmt_bin" -w -- < "$$list"; then \
		echo "gofmt failed — see the parse errors above; not all files were formatted" >&2; \
		exit 1; \
	fi

fmt-check:
	@$(resolve_gofmt); \
	$(collect_go_files); \
	if ! unformatted="$$(xargs -0 "$$gofmt_bin" -l -- < "$$list")"; then \
		echo "gofmt failed — see the parse errors above; the formatting gate did not complete" >&2; \
		exit 1; \
	fi; \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted Go files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	go vet ./...

test:
	go test ./...

DEV_STATIC_INPUTS := $(wildcard go-static-checks.md scripts/go_static_checks.py tests/test_go_static_interrupt.py)
ci: public-surface-check fmt-check vet test $(if $(DEV_STATIC_INPUTS),exhaustive-check static-checks-test)
	go build ./...

public-surface-check: export-dryrun
	python3 tests/test_public_surface.py

export-dryrun:
	python3 scripts/export_public.py

clean:
	rm -rf bin dist

ifneq ($(DEV_STATIC_INPUTS),)
.PHONY: exhaustive-check nilaway-report static-checks-test
exhaustive-check:
	python3 scripts/go_static_checks.py exhaustive "$(GO)"

nilaway-report:
	python3 scripts/go_static_checks.py nilaway "$(GO)"

static-checks-test:
	python3 tests/test_go_static_interrupt.py scripts/go_static_checks.py
endif
