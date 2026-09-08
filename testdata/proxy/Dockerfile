FROM docker:29-dind
RUN apk add --no-cache openssh-server curl
COPY ship /usr/local/bin/ship
COPY sample-app /fixture/sample-app
COPY halfclose /fixture/halfclose
COPY probe /fixture/probe
COPY authorized_keys /root/.ssh/authorized_keys
COPY entrypoint.sh /fixture/entrypoint.sh
RUN chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys && chmod 755 /fixture/entrypoint.sh \
    && printf 'Port 22\nPermitRootLogin prohibit-password\nPasswordAuthentication no\nAllowTcpForwarding yes\n' > /etc/ssh/sshd_config
ENV DOCKER_TLS_CERTDIR=""
ENTRYPOINT ["/fixture/entrypoint.sh"]
