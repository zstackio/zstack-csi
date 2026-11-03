# xfsprogs and e2fsprogs come from the base image; their versions decide which
# filesystem features new volumes get, so change the base image deliberately.
FROM debian:12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends e2fsprogs xfsprogs util-linux mount \
 && rm -rf /var/lib/apt/lists/*

ARG TARGETARCH
COPY _output/linux-${TARGETARCH}/zstack-csi /bin/zstack-csi

ENTRYPOINT ["/bin/zstack-csi"]
