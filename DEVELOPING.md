# Development guides and tips  

## Before Posting PR

Developers are expected to run the following prior to posting PRs.  

```shell
make ci-job
```

This will run the various linters, as well as required unit tests.  

## Linting

### Run `golangci-lint`

Full list linters and config can be found in `.golangci.yml`

- The golangci-lint test can be run individually at any time during development (Note: `make ci-job` also runs this)

  ```shell
  make golangci-lint
  ```

- Or run the following to allow `golangci-lint` for any auto fixes (if available by linter)
  
  ```shell
  golangci-lint run --fix --config .golangci.yml
  ```

### Suppress `golangci-lint` errors

If you would like to suppress a false positive, consider in-line syntax. Please note that such suppressions should only be used in exceptional cases, however, rather than common practice.

An example error for gosec

```console
lca-cli/ops/ops.go:162:17: G107: Potential HTTP request made with variable url (gosec)
                        resp, err := http.Get(healthzEndpoint)
```

Suppress the error with the following syntax `//nolint:mylinter1,mylinter2`.

```go
resp, err := http.Get(healthzEndpoint) //nolint:gosec
```
  
### Reference

- List of linters [here](https://golangci-lint.run/usage/linters/). See `AutoFix` column for which ones can work with `--fix`.
- More techniques for suppressing false positives [here](https://golangci-lint.run/usage/false-positives/)

## Must-Gather

Consider checking out the [enhancement](https://github.com/openshift/enhancements/blob/master/enhancements/oc/must-gather.md) for latest features/tips and The OCP [must-gather](https://github.com/openshift/must-gather) repo for examples.  

Run locally  

```shell
# $1 - path to local output dir. It will created if not present
./must-gather/collection-scripts/gather must-gather/tmp
```
