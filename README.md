# {Beat}

Welcome to {Beat}.

Ensure that this folder is at the following location:
`${GOPATH}/src/github.com/thetherington/routebeat`

## Getting Started with {Beat}

### Requirements

* [Golang](https://golang.org/dl/) 1.26
* Docker and Python 3.12 to package (see [Building on Beats 8.19.25](#building-on-beats-81925))

### Init Project
To get running with {Beat} and also install the
dependencies, run the following command:

```
make setup
```

It will create a clean git history for each major step. Note that you can always rewrite the history if you wish before pushing your changes.

To push {Beat} in the git repository, run the following commands:

```
git remote set-url origin https://github.com/thetherington/routebeat
git push origin master
```

For further development, check out the [beat developer guide](https://www.elastic.co/guide/en/beats/libbeat/current/new-beat.html).

### Build

To build the binary for {Beat} run the command below. This will generate a binary
in the same directory with the name routebeat.

```
make
```


### Run

To run {Beat} with debugging output enabled, run:

```
./routebeat -c routebeat.yml -e -d "*"
```


### Test

To test {Beat}, run the following command:

```
make testsuite
```

alternatively:
```
make unit-tests
make system-tests
make integration-tests
make coverage-report
```

The test coverage is reported in the folder `./build/coverage/`

### Update

Each beat has a template for the mapping in elasticsearch and a documentation for the fields
which is automatically generated based on `fields.yml` by running the following command.

```
make update
```


### Cleanup

To clean  {Beat} source code, run the following command:

```
make fmt
```

To clean up the build directory and generated artifacts, run:

```
make clean
```


### Clone

To clone {Beat} from the git repository, run the following commands:

```
mkdir -p ${GOPATH}/src/github.com/thetherington/routebeat
git clone https://github.com/thetherington/routebeat ${GOPATH}/src/github.com/thetherington/routebeat
```


For further development, check out the [beat developer guide](https://www.elastic.co/guide/en/beats/libbeat/current/new-beat.html).


## Building on Beats 8.19.25

Routebeat is built on Elastic Beats **8.19.25** with Go **1.26**. It uses a fork of Beats, [thetherington/beats8](https://github.com/thetherington/beats8) (branch `tropobeat-8.19.25`, tag `v7.9.19`), which carries two changes to libbeat:

- `libbeat/publisher/processing/processors.go` fixes the debug "Publish event" log line.
- `libbeat/mapping/field.go` accepts the `geo_shape` field type (not used by routebeat; harmless).

`go.mod` points at the fork with `replace github.com/elastic/beats/v7 => github.com/thetherington/beats8/v7 v7.9.19`. The module path stays `github.com/elastic/beats/v7`. The fork's tags are used by other beats, so add a new tag for each change and never move an existing one.

Building against the published tag in module mode is enough. A Go workspace is optional and only needed when changing the fork at the same time:

```
routebeat_workspace/
├── go.work        # uses ./beats and ./routebeat
├── beats/         # thetherington/beats8 checkout, branch tropobeat-8.19.25
└── routebeat/     # this repository
```

> **Packaging ignores the workspace.** Packaging builds inside a container that mounts only `routebeat/`, so it can't see `go.work` or `../beats` and uses the fork version in `go.mod`. Run `go mod tidy` with `GOWORK=off` so that `go.mod` matches what packaging builds, and push and tag fork changes before packaging.

After changing `_meta/fields.yml` or the config templates in `_meta/config/`, regenerate `fields.yml`, `include/fields.go`, the docs and the `routebeat*.yml` files:

```bash
PYTHON_EXE=python3.12 mage update
```

> `mage update` **overwrites** `routebeat.yml`, `routebeat.reference.yml` and `routebeat.docker.yml` from the templates. `mage package` and `mage config` do the same. Copy any local edits first, or make them in `_meta/config/`.

> **Python 3.12.** `mage update` sets up Beats' Python test environment, whose pinned packages don't install on Python 3.13 or later. If the system Python is newer, install 3.12 (for example `uv python install 3.12`) and set `PYTHON_EXE=python3.12`. If the environment was already created with the wrong Python, delete `build/python-env` first.

> **Don't use `mage fmt` or `mage check` on the Python files.** Beats pins an old autopep8 that, under Python 3.12, corrupts f-strings. If either command was run, revert its changes to `*.py` files.

## Packaging

```bash
BEAT_GO_VERSION=1.26.11 PYTHON_EXE=python3.12 PLATFORMS=linux/amd64 mage package
git checkout -- routebeat.yml    # package runs update, which resets it
```

This builds the tar.gz, deb, rpm and two Docker images (Ubuntu and Wolfi) in `build/distributions/`. Omit `PLATFORMS` to build every platform. Packaging needs Docker. The binary is compiled in Elastic's `golang-crossbuild` image, on Debian 11, so it runs on older Linux (it needs nothing newer than glibc 2.3.3). Don't put a binary from `mage build` into a package: a host build needs the host's newer glibc.

**Go version.** Without `BEAT_GO_VERSION`, packaging uses the Go version Beats asks for (1.26.10) and Elastic's image for it. Go 1.26.11 fixes GO-2026-6859 (`net/http`). Until Elastic publishes a 1.26.11 image, packages are built with a local stand-in: Elastic's 1.26.10 image with Go replaced by the official release, tagged with the name the packaging step looks for:

```dockerfile
# docker build -t docker.elastic.co/beats-dev/golang-crossbuild:1.26.11-main-debian11 .
# Download go1.26.11.linux-amd64.tar.gz from go.dev/dl into the build folder first, and check it:
# sha256 9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec
FROM docker.elastic.co/beats-dev/golang-crossbuild:1.26.10-main-debian11
COPY go1.26.11.linux-amd64.tar.gz /tmp/go.tgz
RUN rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
LABEL org.opencontainers.image.description="LOCAL BUILD: golang-crossbuild 1.26.10-main-debian11 with Go upgraded to official go1.26.11"
```

When Elastic publishes their own image, remove the local one (`docker rmi docker.elastic.co/beats-dev/golang-crossbuild:1.26.11-main-debian11`) and package again.

**Check every package before release.** Confirm the Go version, the fork version and the libraries that were actually compiled in:

```bash
tar -xzf build/distributions/routebeat-oss-*-linux-x86_64.tar.gz -C /tmp
go version -m /tmp/routebeat-*-linux-x86_64/routebeat | grep -E 'go1\.|beats8|x/crypto|x/net'
go run golang.org/x/vuln/cmd/govulncheck@latest -mode=binary /tmp/routebeat-*-linux-x86_64/routebeat
```

**Deployment.** The data stream name changes to `routebeat-8.19.25`. Load the new index template before starting the beat. Dashboards that read `routebeat-*` need no change.
