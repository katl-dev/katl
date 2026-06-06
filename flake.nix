{
  description = "Katl development shell";

  inputs = {
    nixpkgs.url = "nixpkgs";
  };

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      pkgsFor = system: import nixpkgs { inherit system; };
      shellFor =
        pkgs:
        pkgs.mkShell {
          packages = with pkgs; [
            bashInteractive
            cacert
            coreutils
            cpio
            cryptsetup
            curl
            dnf5
            dosfstools
            e2fsprogs
            erofs-utils
            gawk
            git
            go
            iproute2
            jq
            kubectl
            mkosi
            mtools
            openssl
            OVMFFull
            protobuf
            protoc-gen-go
            qemu_kvm
            rpm
            squashfsTools
            systemd
            util-linux
            xz
            zstd
          ];

          shellHook = ''
            export KATL_OVMF_CODE="''${KATL_OVMF_CODE:-${pkgs.OVMFFull.fd}/FV/OVMF_CODE.fd}"
            export KATL_OVMF_VARS="''${KATL_OVMF_VARS:-${pkgs.OVMFFull.fd}/FV/OVMF_VARS.fd}"
            export KATL_VMTEST_QEMU="''${KATL_VMTEST_QEMU:-${pkgs.qemu_kvm}/bin/qemu-system-x86_64}"
            export KATL_VMTEST_QEMU_IMG="''${KATL_VMTEST_QEMU_IMG:-${pkgs.qemu_kvm}/bin/qemu-img}"
            export KATL_VMTEST_IP="''${KATL_VMTEST_IP:-${pkgs.iproute2}/bin/ip}"
            export KATL_QEMU_BRIDGE_HELPER="''${KATL_QEMU_BRIDGE_HELPER:-${pkgs.qemu_kvm}/libexec/qemu-bridge-helper}"
          '';
        };
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = shellFor pkgs;
          vm = shellFor pkgs;
        }
      );
    };
}
