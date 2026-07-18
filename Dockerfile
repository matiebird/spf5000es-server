FROM golang:1.26.5-alpine AS build
ARG GIT_REVISION=unknown
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildRevision=${GIT_REVISION}" -o /out/spf5000es-server .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /out/spf5000es-server /usr/local/bin/spf5000es-server
WORKDIR /app
ENTRYPOINT ["/usr/local/bin/spf5000es-server"]
