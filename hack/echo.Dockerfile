# Image for the lab's echo backend. The binary is built on the host as a
# static Linux executable (see hack/lab-spike.sh), so no base image is needed.
FROM scratch
COPY routeparity-echo /routeparity-echo
USER 65532:65532
ENTRYPOINT ["/routeparity-echo"]
