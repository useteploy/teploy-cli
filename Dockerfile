# Smoke-test vehicle for the R01 release-verify built-image check
# (scripts/release-verify.sh smoke). NOT a published release artifact —
# goreleaser ships binaries/archives; this image exists so the exact
# verified binary proves it runs in a minimal (scratch) container.
# The binary is COPY'd from the release-verify dist directory, so the
# image provenance is always the checksummed matrix build.
#
# Build (as the smoke script does):
#   docker build --build-arg BINARY=dist-verify/teploy_linux_<arch> -t teploy:release-smoke .
FROM scratch
ARG BINARY=dist-verify/teploy_linux_amd64
COPY ${BINARY} /teploy
ENTRYPOINT ["/teploy"]
