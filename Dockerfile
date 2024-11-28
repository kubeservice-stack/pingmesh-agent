ARG ARCH="amd64"
ARG OS="linux"

# Build the manager binary
FROM golang:1.22.9-alpine as builder
RUN apk add --no-cache gcc musl-dev libc6-compat

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
COPY main.go main.go
COPY config/ config/
COPY prober/ prober/

#RUN go mod download

COPY vendor/ vendor/

# Build
RUN CGO_ENABLED=0 GOOS=${OS} GOARCH=${ARCH} go build -ldflags "-linkmode external -extldflags -static" -o pingmesh-agent ./

FROM quay.io/prometheus/busybox-${OS}-${ARCH}:latest

COPY --from=builder /workspace/pingmesh-agent  /bin/pingmesh-agent
COPY pingmesh.yml   /etc/pingmesh-agent/pingmesh.yml

EXPOSE      9115
ENTRYPOINT  [ "/bin/pingmesh-agent" ]
CMD         [ "--config.file=/etc/pingmesh-agent/pingmesh.yml" ]
CMD         [ "--pinglist.file=/etc/pingmesh-agent/pinglist.yml" ]
