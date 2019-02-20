#!/bin/bash -ex

rm -rf dest

../octoci build --serialize docker://centos:latest rfses
umoci unpack --image oci:octoci dest
[ -f dest/rootfs/a ]
[ -f dest/rootfs/b ]
[ -f dest/rootfs/dir/c ]
[ -f dest/rootfs/dir/d ]

rm -rf dest

mkdir -p random
for i in `seq 10`; do
    dd if=/dev/urandom of=random/$i bs=1M count=1
done

echo ./random > random/rfslist
../octoci build --serialize --max-layer-size=$((1024*1024*5)) docker://centos:latest random/rfslist
umoci unpack --image oci:octoci dest
for i in `seq 10`; do
    [ "$(sha256sum random/$i | cut -d" " -f1)" = "$(sha256sum dest/rootfs/$i | cut -d" " -f1)" ]
done

manifest=$(cat oci/index.json | jq -r .manifests[0].digest | cut -f2 -d:)
layer=$(cat oci/blobs/sha256/$manifest | jq -r .layers[0].digest | cut -f2 -d:)

for i in $(ls -l oci/blobs/sha256 | grep -v $layer | awk '{print $5}' | tail --lines=+2 | tr '\n' ' '); do
    if [ "$i" -gt "$((1024*1024*5))" ]; then
        echo "a layer is too big" && false
    fi
done
rm -rf random
