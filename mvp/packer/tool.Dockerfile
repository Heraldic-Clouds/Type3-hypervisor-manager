FROM hashicorp/packer:1.16.2 AS packer
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip openssh-client && rm -rf /var/lib/apt/lists/*
COPY --from=packer /bin/packer /bin/packer
RUN curl -fsSL https://s3.amazonaws.com/session-manager-downloads/plugin/latest/ubuntu_64bit/session-manager-plugin.deb -o /tmp/ssm.deb && dpkg -i /tmp/ssm.deb && rm /tmp/ssm.deb
ENV PACKER_PLUGIN_PATH=/plugins
ENTRYPOINT ["/bin/packer"]

