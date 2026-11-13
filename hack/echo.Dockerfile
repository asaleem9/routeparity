# Image for the lab's echo backend. The binary is built outside Docker as a
# static Linux executable, one per architecture, so no base image is needed.
# The build context holds routeparity-echo-amd64 and/or routeparity-echo-arm64;
# BuildKit sets TARGETARCH to the architecture being built.
FROM scratch
ARG TARGETARCH
COPY routeparity-echo-${TARGETARCH} /routeparity-echo
USER 65532:65532
ENTRYPOINT ["/routeparity-echo"]
