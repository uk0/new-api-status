FROM golang:1.24-alpine AS builder
RUN apk add --no-cache gcc musl-dev
ENV GOFLAGS="-mod=mod"
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -ldflags "-s -w" -o new-api-status .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /build/new-api-status /usr/local/bin/
EXPOSE 8787
ENTRYPOINT ["new-api-status"]
