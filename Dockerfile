FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -o /notify-relay .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /notify-relay /usr/local/bin/notify-relay
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://localhost:8080/livez || exit 1
CMD ["notify-relay"]
