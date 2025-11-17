#!/bin/bash

set -e

apt autoremove \
    media-types python3 wireguard-tools alsa-utils bash-completion armbian-config bc \
    bluez bluez-firmware btrfs-progs cron curl debconf-utils debsums dialog dosfstools \
    figlet fping  gpiod htop iputils-ping iw jq openssh-client wireless-regdb wpasupplicant \
    wget ncurses-term toilet toilet-fonts bsdextrautils logrotate lsof man-db psmisc rsync \
    whiptail locales libperl5.40 xkb-data libtirpc-common libtirpc3t64 libyaml-0-2 mtd-utils \
    usbutils sensible-utils readline-common parted less ca-certificates apt-utils openssh-server \
    openssh-sftp-server nano sudo passwd login login.defs sysvinit-utils udev

touch /root/.no_rootfs_resize