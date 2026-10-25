FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends xfce4 xfce4-terminal xrdp xorgxrdp dbus dbus-x11 x11-xserver-utils thunar mousepad fonts-dejavu-core && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/bash mea && echo startxfce4 > /home/mea/.xsession && chown mea:mea /home/mea/.xsession && adduser xrdp ssl-cert
COPY desktop/start.sh /start.sh
ENTRYPOINT ["bash","/start.sh"]
