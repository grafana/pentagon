FROM golang:1.13-alpine as builder

RUN apk add --no-cache ca-certificates libc-dev git make gcc
RUN adduser -D pentagon
RUN mkdir -p /src/pentagon && chown pentagon /src/pentagon
USER pentagon

# Enable go modules
ENV GO111MODULE on

# The golang docker images configure GOPATH=/go
RUN mkdir -p /go/pkg/
COPY --chown=pentagon . /src/pentagon/

WORKDIR /src/pentagon/

RUN make GOMOD_RO_FLAG='-mod=readonly' build/linux/pentagon

FROM alpine
USER root
RUN adduser -D pentagon
RUN apk add --no-cache ca-certificates
RUN mkdir -p /app
COPY --from=builder /src/pentagon/build/linux/pentagon /app/pentagon

# drop privileges again
USER pentagon
ENTRYPOINT ["/app/pentagon"]
