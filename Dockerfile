# syntax=docker/dockerfile:1
# Base images default to the in-cluster artifact registry (buildkitd trusts it
# as an insecure registry); override with --build-arg when building elsewhere.
ARG REGISTRY=forgejo.develop.10.199.64.20.nip.io/root
FROM ${REGISTRY}/golang:1.26-alpine AS build
ARG HTTP_PROXY=http://mihomo.develop.svc.cluster.local:7890
ARG HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc,.nip.io \
    GOINSECURE=forgejo.develop.10.199.64.20.nip.io \
    GOPRIVATE=forgejo.develop.10.199.64.20.nip.io \
    GOPROXY=http://artifact.zergx.svc.cluster.local/pkgs/go \
    GOSUMDB=off \
    GONOSUMDB=abep.dev/sdk,abep.dev/sdk/nats,abep.dev/sdk/ws \
    GONOSUMCHECK=1 \
    GOFLAGS=-mod=mod
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
    && apk add --no-cache git \
    && git config --global http.sslVerify false \
    && git config --global url."https://root:devpassword@forgejo.develop.10.199.64.20.nip.io/".insteadOf "https://forgejo.develop.10.199.64.20.nip.io/"
WORKDIR /build
COPY go.mod go.sum ./
COPY *.go ./
# manifest.yaml is embedded into the binary via go:embed — no sidecar needed.
COPY manifest.yaml ./
RUN CGO_ENABLED=0 go build -o /out/repo-extension .

FROM ${REGISTRY}/alpine:3.24
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
    && apk add --no-cache ca-certificates
COPY --from=build /out/repo-extension /usr/local/bin/repo-extension
EXPOSE 8080
ENTRYPOINT ["repo-extension"]
