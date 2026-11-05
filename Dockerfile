# God's Eye web app: docker run -p 8080:8080 -v godseye:/data -e ANTHROPIC_API_KEY=... gods-eye
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gods-eye .

FROM alpine:3.20
RUN apk add --no-cache git ca-certificates && adduser -D -h /data godseye
USER godseye
COPY --from=build /gods-eye /usr/local/bin/gods-eye
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["gods-eye", "serve", "--addr", ":8080", "--data", "/data"]
