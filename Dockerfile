FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/build ./web/build
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /dockercd ./cmd/dockercd

FROM alpine:3.22
# ssh: Docker over ssh:// for remote Hosts later; git repos over ssh go through go-git
RUN apk add --no-cache ca-certificates openssh-client
COPY --from=go /dockercd /usr/local/bin/dockercd
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["dockercd"]
