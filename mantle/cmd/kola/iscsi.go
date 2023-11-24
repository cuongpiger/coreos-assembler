// Copyright 2023 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"github.com/coreos/coreos-assembler/mantle/platform/conf"
)

// from https://github.com/jbtrystram/targetcli-containers/tree/main/ignition
var iscsiTargetConfig = conf.Butane(`
variant: fcos
version: 1.5.0
storage:
  files:
    - path: /etc/containers/systemd/target.container
      contents:
        inline: |
            [Unit]
            Description=Targetd container
            Documentation=https://github.com/jbtrystram/targetcli-containers
            After=local-fs.target
            After=network-online.target, dev-disk-by\x2did-virtio\x2dtarget.device
            Wants=network-online.target
            [Container]
            Image=quay.io/jbtrystram/targetcli:latest
            ContainerName=target
            AddCapability=CAP_SYS_MODULE
            Network=host
            Volume=/dev/disk/by-id/virtio-target:/dev/disk/by-id/virtio-target
            Volume=/lib/modules:/lib/modules
            Volume=/sys/kernel/config:/sys/kernel/config
            PodmanArgs=--privileged
            [Install]
            # Start by default on boot
            WantedBy=multi-user.target
            [Service]
            # Extend Timeout to allow time to pull the image
            TimeoutStartSec=900
    - path: /usr/local/bin/targetcli_script
      mode: 0755
      contents:
          inline: |
            #!/bin/bash
            #mkfs.ext4 /dev/disk/by-id/virtio-target -L iscsiboot
            podman exec target bash -exc "
            targetcli /backstores/block create name=coreos dev=/dev/disk/by-id/virtio-target
            targetcli iscsi/ create iqn.2023-10.coreos.target.vm:coreos
            targetcli iscsi/iqn.2023-10.coreos.target.vm:coreos/tpg1/luns create /backstores/block/coreos
            targetcli iscsi/iqn.2023-10.coreos.target.vm:coreos/tpg1/ set attribute authentication=0 demo_mode_write_protect=0 generate_node_acls=1 cache_dynamic_acls=1
            "
            # Will return 0 if the discovery yield a valid portal
            iscsiadm -m discovery -p 127.0.0.1 -t st | grep iqn.2023-10.coreos.target.vm:coreos
    - path: /mnt/temp/boot.ipxe
      mode: 0644
      contents:
        inline: |
            #!ipxe
            set initiator-iqn iqn.2023-11.coreos.diskless:testsetup
            sanboot iscsi:10.0.2.15::::iqn.2023-10.coreos.target.vm:coreos
    - path: /usr/local/bin/install-coreos-iscsi
      mode: 0755
      contents:
        inline: |
          #!/bin/bash
          set -euxo
          # Mount the iscsi target
          iscsiadm -m discovery -t st -p 127.0.0.1
          iscsiadm -m node -T iqn.2023-10.coreos.target.vm:coreos -l
          # Install coreos
          # FIXME How are we sure this is the iscsi mounted disk ?
          # (if it's sda it should be, because virtio disks are usually /dev/vda)
          coreos-installer install /dev/sda --append-karg rd.iscsi.firmware=1 --append-karg ip=ibft
          # Unmount the disk
          iscsiadm --mode node --logoutall=all
    - path: /usr/local/bin/boot-coreos-iscsi-vm
      mode: 0755
      contents:
        inline: |
          #!/bin/bash
          set -euxo
          # start coreOS through COSA
          # FIXME : make a .container unit out of this ?
          podman run -ti --privileged --net=host --name iscsiboot --rm \
              -v /mnt/temp/boot.ipxe:/mnt/temp/boot.ipxe \
              -v /mnt/workdir:/mnt/workdir \
              quay.io/coreos-assembler/coreos-assembler shell \
              -- kola qemuexec --netboot /mnt/temp/boot.ipxe --usernet-addr 10.0.3.0/24
          # wait a little bit to let it boot
          sleep 30
          # verify successful boot
          #journalctl --unit iscsi-boot  -g '.*OK.*multi-user\.target' -o cat -q
          podman logs iscsiboot | grep '.*OK.*multi-user\.target'
          # Propagate success
          /usr/bin/echo "iscsi-boot-ok" >/dev/virtio-ports/testisocompletion
systemd:
    units:
    - name: setup-targetcli.service
      enabled: true
      contents: |
        [Unit]
        Description=Setup targetcli
        Requires=target.service
        After=target.service
        ConditionFirstBoot=true
        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/usr/local/bin/targetcli_script
        [Install]
        WantedBy=multi-user.target
    - name: install-coreos-to-iscsi-target.service
      enabled: true
      contents: |
        [Unit]
        Description=Mount an iscsi target and install coreOS into it
        Requires=setup-targetcli.service
        After=setup-targetcli.service
        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/usr/local/bin/install-coreos-iscsi
        [Install]
        WantedBy=multi-user.target
    - name: boot-iscsi-coreos-vm.service
      enabled: true
      contents: |
          [Unit]
          Description=Boot a coreOS VM through cosa from an iscsi target
          Requires=install-coreos-to-iscsi-target.service
          After=install-coreos-to-iscsi-target.service
          [Service]
          Type=oneshot
          RemainAfterExit=yes
          ExecStart=/usr/local/bin/boot-coreos-iscsi-vm
          [Install]
          WantedBy=multi-user.target`)
