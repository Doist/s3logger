FROM public.ecr.aws/docker/library/golang:alpine AS builder
RUN apk add git
WORKDIR /app
ENV CGO_ENABLED=0 GOFLAGS="-ldflags=-w -trimpath"
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o s3logger

FROM scratch
COPY --from=builder /app/s3logger /bin/s3logger
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
VOLUME /var/spool/s3logger
CMD ["/bin/s3logger"]
