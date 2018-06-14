#!/bin/bash -ex

rm -rf dest

../octoci build docker://centos:latest rfses
umoci unpack --image oci:octoci dest
[ -f dest/rootfs/a/a ]
[ -f dest/rootfs/b/b ]
