## Prepare

1. `podman build -t fuse-debian ./tools`
2. Build the builder
   - `podman run -e CGO_ENABLED=0 --rm -v $(pwd)/:/work fuse-debian go build -buildvcs=false -ldflags='-s -w -extldflags "-static"' -trimpath -o ./tools/bin/builder ./tools/builder`
3. Build tezsign gadget binary
  `podman run -e GOOS=linux -e GOARCH=arm64 -e CGO_ENABLED=1 --rm -v $(pwd)/:/work fuse-debian go build -buildvcs=false -ldflags='-s -w -extldflags "-static"' -trimpath -o ./tools/builder/assets/tezsign ./app/gadget`
4. Build tezsign registrar
  `podman run -e GOOS=linux -e GOARCH=arm64 --rm -v $(pwd)/:/work fuse-debian go build -buildvcs=false -ldflags='-s -w -extldflags "-static"' -trimpath -o ./tools/builder/assets/ffs_registrar ./app/ffs_registrar`
5. `podman run --privileged -v $(pwd):/work -w /work -it fuse-debian`

## BUILD IMAGE

1. Download image you want to patch into `imgs`. Right now RPi and Radxa Zero 3 Armbian IoT images should work
2. Run `./tools/bin/builder imgs/<source img> imgs/<new image>`, for example:
    - `./tools/bin/builder imgs/Armbian_25.8.1_Rpi4b_bookworm_current_6.12.41_minimal.img imgs/Armbian_25.8.1_Rpi4b_bookworm_current_6.12.41_minimal.new.img.xz`
    OR 
    - `./tools/bin/builder imgs/Armbian_community_25.11.0-trunk.334_Radxa-zero3_trixie_vendor_6.1.115_minimal.img imgs/Armbian_community_25.11.0-trunk.334_Radxa-zero3_trixie_vendor_6.1.115_minimal.new.img.xz`
    
3. Produced image is **compressed** and ready to be burned to sdcard.

## TEST IMAGE
- rootfs and /app are readonly 
You can mount them rw with:
  - `sudo mount -o remount,rw /`
  - `sudo mount -o remount,rw /app`


## Reviewing builder changes
You can keep generated FS mounted by setting `DISABLE_UNMOUNTS` to true in `constants.go`. But before rebuild image again you should unmount directories manually:

```
fusermount -u /tmp/tezsign_image_builder/boot
fusermount -u /tmp/tezsign_image_builder/rootfs
```

TODO: move udev to main readme
## udev rules (DEV image only)

To expose dev interface you need to setup udev rules:

```
# /etc/udev/rules.d/50-usb-gadget.rules
SUBSYSTEM=="net", ACTION=="add", ATTR{address}=="ae:d3:e6:cd:ff:f3", NAME="tezsign_dev"
```

```
# /etc/udev/rules.d/99-usb-network.rules
SUBSYSTEM=="net", ACTION=="add", ATTR{address}=="ae:d3:e6:cd:ff:f3", RUN+="/usr/bin/ip addr add 10.10.10.2/24 dev tezsign_dev", RUN+="/usr/bin/ip link set dev tezsign_dev up"
```

Then you get SSH access to gadget through `dev@10.10.10.1` with password `tezsign`.