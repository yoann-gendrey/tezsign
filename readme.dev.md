# tezsign Developer Guide

This document provides instructions for developers who want to build, troubleshoot, or contribute to `tezsign`.

## ❗ Developer (`dev`) Images

For development and debugging, `tezsign` provides special `dev` flavor images. These images are **insecure by design** and **must never be used in a production environment**.

### `dev` Image Features

`dev` images are built on top of the production images but include several key additions to facilitate development:

1.  **`dev` User Account:** A `dev` account is created with the password `tezsign`.
2.  **Sudo Access:** The `dev` user has full `sudoers` permissions, allowing root access.
3.  **SSH Server:** The image automatically starts an SSH server on boot.
4.  **ECM Gadget:** In addition to the standard `tezsign` USB gadget, the `dev` image enables an **ECM (Ethernet Control Model) gadget**. This creates a USB Ethernet interface, allowing you to SSH into the device from your host machine.

### Host Machine Setup (Linux)

For your host machine to recognize and configure the USB Ethernet (ECM) gadget, you must create two `udev` rules. This allows your host to automatically assign an IP address to its side of the USB connection, enabling you to reach the `tezsign` gadget.

The gadget's `dev` image is pre-configured with the static IP address `10.10.10.1`. We will configure the host to have `10.10.10.2`.

**1. Create the interface naming rule:**

This rule identifies the `tezsign` gadget by its MAC address and gives it a stable network interface name (`tezsign_dev`).

```bash
# /etc/udev/rules.d/50-usb-gadget.rules
SUBSYSTEM=="net", ACTION=="add", ATTR{address}=="ae:d3:e6:cd:ff:f3", NAME="tezsign_dev"
```

**2. Create the network configuration rule:**

This rule runs when the `tezsign_dev` interface is added. It assigns the IP address `10.10.10.2/24` to the host's side of the connection and brings the interface up.

```bash
# /etc/udev/rules.d/99-usb-network.rules
SUBSYSTEM=="net", ACTION=="add", ATTR{address}=="ae:d3:e6:cd:ff:f3", RUN+="/usr/bin/ip addr add 10.10.10.2/24 dev tezsign_dev", RUN+="/usr/bin/ip link set dev tezsign_dev up"
```

**3. Reload `udev` Rules**

```bash
sudo udevadm control --reload-rules
sudo udevadm trigger
```

### Accessing the Gadget

Once your `udev` rules are in place and the `tezsign` gadget is connected, you can SSH into it from your host machine:

```bash
ssh dev@10.10.10.1
```

The password is: `tezsign`

You will now have a full shell on the `tezsign` gadget with `sudo` access, allowing you to inspect logs, test services, and debug the application.

### Working with the Read-Only Filesystem

By default, all partitions on the device (except for `/data`) are mounted as **read-only** for security. Partition layouts may differ between devices. You can inspect all current mount points and their state (like `ro` for read-only) by running:

```bash
mount | grep " ro,"
```

If you need to make changes to a read-only partition (e.g., to modify a file on the root filesystem), you must first remount it as read-write.

Use the following command template:
```bash
sudo mount -o remount,rw <path-to-mount-point>
```

For example, to make the root filesystem (`/`) read-write, run:
```bash
sudo mount -o remount,rw /
```
You can now edit files on the root partition.

Once you are finished with your changes, the easiest way to return the device to its read-only state is to simply **reboot** it.
```bash
sudo reboot
```