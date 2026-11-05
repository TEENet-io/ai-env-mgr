# tenant-admin image. Build the static binary first, then the image:
#
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o cmd/tenant-admin/tenant-admin ./cmd/tenant-admin
#   docker build -t tenant-admin:<version> cmd/tenant-admin
#
# No shell, no package manager: the binary is the whole image. It runs as the
# gateway host's ec2-user (1000), who owns the mounted /config directory.
FROM scratch
COPY tenant-admin /usr/local/bin/tenant-admin
ENV PATH=/usr/local/bin
USER 1000:1000
EXPOSE 4100
ENTRYPOINT ["tenant-admin"]
CMD ["serve"]
