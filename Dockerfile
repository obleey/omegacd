# Build stages run natively on the build machine and cross-compile for the target platform.
FROM --platform=$BUILDPLATFORM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS go
ARG VERSION=""
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/build ./web/build
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X github.com/obleey/omegacd/internal/version.Version=${VERSION}" \
    -o /omegacd ./cmd/omegacd

FROM alpine:3.24
ARG VERSION=""
LABEL org.opencontainers.image.title="OmegaCD" \
      org.opencontainers.image.description="GitOps continuous delivery for Docker Compose" \
      org.opencontainers.image.source="https://github.com/obleey/omegacd" \
      org.opencontainers.image.url="https://github.com/obleey/omegacd" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
# openssh-client: the Docker client dials ssh:// Hosts through the ssh binary; Git over SSH goes through go-git
RUN apk add --no-cache ca-certificates openssh-client
COPY --from=go /omegacd /usr/local/bin/omegacd
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["omegacd"]
