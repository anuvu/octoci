#!/bin/bash -ex

rm -rf dest

../octoci build --serialize docker://centos:latest rfses
umoci unpack --image oci:octoci dest
[ -f dest/rootfs/a ]
[ -f dest/rootfs/b ]
[ -f dest/rootfs/dir/c ]
[ -f dest/rootfs/dir/d ]
