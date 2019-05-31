ARG GO_VERSION=1.11.2
ARG ALPINE_VERSION=3.8

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

# Git is required for fetching the dependencies.
RUN apk add --no-cache git

# Set the working directory outside $GOPATH to enable the support for modules.
WORKDIR /src

# Fetch dependencies first; they are less susceptible to change on every build
# and will therefore be cached for speeding up the next build
COPY ./go.mod ./
RUN go mod download

# Import the code from the context.
COPY ./ ./

# Build the executable to `/app`. Mark the build as statically linked.
RUN CGO_ENABLED=0 go build \
    -installsuffix 'static' \
    -o /s3logger .

# Final stage: the running container.
FROM alpine:${ALPINE_VERSION} AS final

# Create the user and group files that will be used in the running container to
# run the process as an unprivileged user.
RUN addgroup -S s3logger && \
    adduser -D -S -h /data -G s3logger s3logger && \
    mkdir /var/spool/s3logger  && chown s3logger /var/spool/s3logger

# Import the compiled executable from the first stage.
COPY --from=builder /s3logger /app/

# Perform any further action as an unprivileged user.
USER s3logger:s3logger

VOLUME [ "/var/spool/s3logger" ]

# Run the compiled binary.
ENTRYPOINT [ "/app/s3logger" ]
