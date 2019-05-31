FROM golang:alpine AS builder
WORKDIR /app
ENV GOPROXY=https://proxy.golang.org
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -o s3logger

FROM scratch
COPY --from=builder /app/s3logger /bin/s3logger
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
VOLUME /var/spool/s3logger
CMD ["/bin/s3logger"]
