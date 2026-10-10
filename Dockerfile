FROM golang:1.27.2-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/index-translate-proxy .

FROM alpine:3.24

RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/index-translate-proxy /usr/local/bin/index-translate-proxy

# The container must listen on all its interfaces so that published ports
# reach the proxy; expose it to the LAN only through the port mapping.
USER nobody
EXPOSE 8080

ENTRYPOINT ["index-translate-proxy"]
CMD ["--serve", "--host", "0.0.0.0"]
