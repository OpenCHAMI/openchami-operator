# Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
#
# SPDX-License-Identifier: MIT

FROM gcr.io/distroless/static:nonroot

WORKDIR /

# GoReleaser provides the prebuilt artifact binary in the docker build context.
COPY operator /usr/local/bin/operator

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/operator"]
