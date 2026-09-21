# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.su[m] ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a static binary with no libc, which is what lets
# the final image be FROM scratch. -s -w strips debug info, -trimpath
# removes local filesystem paths from the binary.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /seatbelt ./cmd/gateway

# scratch has no shell, no package manager, no libc, so there is nothing
# in the image to exploit. The trade is no docker exec, swap for
# alpine:3 if you want a shell in the container.
FROM scratch

# scratch has no root certificates, without this every HTTPS call fails.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /seatbelt /seatbelt

# Numeric since scratch has no /etc/passwd to resolve a username against.
USER 65534:65534

EXPOSE 8080

# docker stop sends SIGKILL 10s after SIGTERM by default, but Seatbelt
# takes up to 20s to drain in-flight requests. Use
# docker stop -t 30 seatbelt to let it finish.
STOPSIGNAL SIGTERM

ENTRYPOINT ["/seatbelt"]
