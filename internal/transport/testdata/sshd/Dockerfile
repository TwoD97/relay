FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    useradd -m -s /bin/bash relay && \
    echo 'relay:relay-smoke-only' | chpasswd && \
    ssh-keygen -A && \
    mkdir -p /run/sshd && \
    printf 'PasswordAuthentication yes\nKbdInteractiveAuthentication no\nPermitRootLogin no\nAllowUsers relay\nPrintMotd no\nUsePAM no\n' > /etc/ssh/sshd_config.d/relay.conf
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
