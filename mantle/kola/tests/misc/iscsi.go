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

package misc

import (
	"context"
	"fmt"
	"time"

	"github.com/coreos/coreos-assembler/mantle/kola/cluster"
	"github.com/coreos/coreos-assembler/mantle/kola/register"
	"github.com/coreos/coreos-assembler/mantle/platform"
	"github.com/coreos/coreos-assembler/mantle/platform/conf"
	"github.com/coreos/coreos-assembler/mantle/platform/machine/qemu"
)

func init() {
	register.RegisterTest(&register.Test{
		Name:        "coreos.iscsi.boot",
		Description: "Verify coreOS will run from an iscsi boot volume.",
		Run:         testiscsiBoot,
		ClusterSize: 0,
		Platforms:   []string{"qemu"},
		Tags:        []string{"ignition"},
	})
}

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
            ConditionPathExists=/var/mnt/workdir-tmp
            After=network-online.target
            Wants=network-online.target
            [Container]
            Image=quay.io/jbtrystram/targetcli:latest
            ContainerName=target
            AddCapability=CAP_SYS_MODULE
            Network=host
            Volume=/lib/modules:/lib/modules
            Volume=/sys/kernel/config:/sys/kernel/config
            Volume=/var/mnt/workdir-tmp/:/var/mnt/workdir-tmp/
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
            fallocate -l 10G /var/disk.img 
            mkfs.ext4 -F /var/disk.img -L iscsiboot
            podman exec target bash -exc "
            targetcli /backstores/fileio create coreos /var/disk.img
            targetcli iscsi/ create iqn.2023-10.coreos.target.vm:coreos
            targetcli iscsi/iqn.2023-10.coreos.target.vm:coreos/tpg1/luns create /backstores/fileio/coreos
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
    - path: /usr/local/bin/bootup-validator
      mode: 0755
      contents:
        inline: |
            #!/bin/bash
            journalctl --unit iscsi-boot  -g '.*OK.*multi-user\.target' -o cat -q
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
        `)

type IscsiTargetServer struct {
  Machine    platform.Machine
  Address    string
}


  // 0 - boot a liveISO VM
	// 1 - create empty block device
	// 2 - share it with iSCSI
func setupTargetMachine(c cluster.TestCluster) IscsiTargetServer {
    var m platform.Machine
    var err error
    var iscsiAddress string

    // TODO Do we want to have both VMs at the same level ? 
    // Forwarding the iSCSI port to the host would achieve no nesting 
    options := platform.QemuMachineOptions{
      
      HostForwardPorts: []platform.HostForwardPort{
        {Service: "ssh", HostPort: 0, GuestPort: 22},
      },
    }
          
    switch pc := c.Cluster.(type) {
    // These cases have to be separated because when put together to the same case statement
    // the golang compiler no longer checks that the individual types in the case have the
    // NewMachineWithQemuOptions function, but rather whether platform.Cluster
    // does which fails
    case *qemu.Cluster:
      m, err = pc.NewMachineWithQemuOptions(iscsiTargetConfig, options)
      for _, hfp := range options.HostForwardPorts {
        if hfp.Service == "tang" {
          iscsiAddress = fmt.Sprintf("10.0.2.2:%d", hfp.HostPort)
        }
      }
    default:
      m, err = pc.NewMachine(iscsiTargetConfig)
      iscsiAddress = fmt.Sprintf("%s:80", m.PrivateIP())
    }
    if err != nil {
      c.Fatal(err)
    }

    // at this point we have a booted machine exposing an iSCSI target
    return IscsiTargetServer {
      Machine:    m,
      Address:    iscsiAddress,
    }
  }

func testiscsiInstall(c cluster.TestCluster) {
    var targetVM = setupTargetMachine(c)

    // Mount the iscsi target locally
    // TODO we could do that through a systemd oneshot unit in iscsiTargetConfig
    targetVM.Machine.SSH("sudo iscsiadm -m discovery -t st -p "+ targetVM.Address)
    targetVM.Machine.SSH("sudo  iscsiadm -m node -T iqn.2023-10.coreos.target.vm:coreos -l")

    // 3 - coreOS Installer into it with kargs ibft
    // install destination : /dev/by-label/iscsiboot
    targetVM.Machine.SSH("coreos-installer -kargs rd.iscsi.firmware=1 ip=ibft /dev/by-label/iscsiboot")

    // 3 bis - unmount iscsi target
    targetVM.Machine.SSH("sudo iscsiadm --mode node --logoutall=all")

    // 4 - boot inner VM from it
    targetVM.Machine.SSH("podman run -ti --privileged --net=host --name iscsiboot --rm -v /mnt/temp/boot.ipxe:/mnt/temp/boot.ipxe quay.io/coreos-assembler/coreos-assembler shell -- kola qemuexec --netboot /mnt/temp/boot.ipxe --usernet-addr 10.0.3.0/24")

    // 5 - check wether the nested VM booted successfully
    targetVM.Machine.SSH("sudo podman logs iscsiboot | grep reached multi-user.target" )

  if err != nil {
    c.Fatalf("Unable to start Tang container: %v\n%s", err, string(errMsg))
  }

  // Wait a little bit for the container to start
  if err := util.Retry(10, time.Second, func() error {
    cmd := fmt.Sprintf("sudo podman exec %s /usr/bin/tang-show-keys", string(containerID))
    thumbprint, _, err = m.SSH(cmd)
    if err != nil {
      return err
    }
    if string(thumbprint) == "" {
      return fmt.Errorf("tang-show-keys returns nothing")
    }
    return nil
  }); err != nil {
    c.Fatalf("Unable to retrieve Tang keys: %v", err)
  }

	builder := platform.NewQemuBuilder()

	defer builder.Close()

	// completionChannel, err := builder.VirtioChannelRead("iscsicompletion")
	// if err != nil {
	// 	return 0, errors.Wrapf(err, "setting up virtio-serial channel")
	// }

	// 3 - boot a VM with the above butane config

	builder.SetConfig(config)

	builder.MemoryMiB = 2048
	inst, err := builder.Exec()
	if err != nil {
		c.Fatal(err.Error())
	}

  machine = c
  inst.SSHAddress()

	_, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	defer inst.Destroy()

	// mach, err := builder.Exec()
	// if err != nil {
	// 	return 0, errors.Wrapf(err, "running iso")
	// }
	// defer mach.Destroy()

	// return awaitCompletion(ctx, mach, outdir, completionChannel, nil, []string{liveOKSignal})
}
