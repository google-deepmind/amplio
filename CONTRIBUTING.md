# How to contribute

We'd love to accept your patches and contributions to this project.

## Before you begin

### Sign our Contributor License Agreement

Contributions to this project must be accompanied by a
[Contributor License Agreement](https://cla.developers.google.com/about) (CLA).
You (or your employer) retain the copyright to your contribution; this simply
gives us permission to use and redistribute your contributions as part of the
project.

If you or your current employer have already signed the Google CLA (even if it
was for a different project), you probably don't need to do it again.

Visit <https://cla.developers.google.com/> to see your current agreements or to
sign a new one.

### Review our community guidelines

This project follows
[Google's Open Source Community Guidelines](https://opensource.google/conduct/).

## Contribution process

### Code reviews

All submissions, including submissions by project members, require review. We
use GitHub pull requests for this purpose. Consult
[GitHub Help](https://help.github.com/articles/about-pull-requests/) for more
information on using pull requests.

### Checks

Run `make test` and `make lint` before sending a pull request. `make lint` runs
golangci-lint with `.golangci.yml`; the linter is a Go tool of its own module
(`tools/golangci-lint.mod`), so it needs nothing installed beyond Go.

Every pull request also runs the `ci` workflow (`.github/workflows/ci.yml`):
the frontend's unit tests, type check and build, then `make vet`,
`make test-race` and `make lint`. It runs on CI's machines rather than yours,
so it catches tests that depend on timing or on how a machine is set up.
