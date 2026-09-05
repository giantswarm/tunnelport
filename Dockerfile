# The manager binary is built by CircleCI (architect/go-build) for every target
# platform and attached to the build context as tunnelport-<os>-<arch>; this
# image only packages it. Compiling it here again (the former builder stage)
# cost ~5 minutes per image build. For a local build, produce the binary first:
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o tunnelport-linux-amd64 .
# hack/smoke/run.sh does this when the binary is missing.
#
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
ARG TARGETOS
ARG TARGETARCH
COPY tunnelport-${TARGETOS}-${TARGETARCH} /manager
USER 65532:65532

ENTRYPOINT ["/manager"]
