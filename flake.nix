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
      katlctlFor =
        pkgs:
        pkgs.writeShellScriptBin "katlctl" ''
          repo_root="$(${pkgs.git}/bin/git rev-parse --show-toplevel 2>/dev/null)" || {
            echo "error: katlctl must be run from the Katl checkout" >&2
            exit 1
          }
          exec ${pkgs.go}/bin/go run "$repo_root/cmd/katlctl" "$@"
        '';
      katldevFor =
        pkgs:
        pkgs.writeShellScriptBin "katldev" ''
          repo_root="$(${pkgs.git}/bin/git rev-parse --show-toplevel 2>/dev/null)" || {
            echo "error: katldev must be run from the Katl checkout" >&2
            exit 1
          }
          exec ${pkgs.go}/bin/go run "$repo_root/cmd/katldev" "$@"
        '';
      shellFor =
        pkgs:
        pkgs.mkShell {
          packages =
            (with pkgs; [
              bashInteractive
              cacert
              cpio
              curl
              dosfstools
              erofs-utils
              git
              go
              jq
              kubectl
              libvirt
              mtools
              OVMFFull
              openssh
              podman
              protobuf
              protoc-gen-go
              protoc-gen-go-grpc
              qemu_kvm
              rpm
              squashfsTools
              systemdUkify
              util-linux
              xorriso
              zstd
            ])
            ++ [
              (katlctlFor pkgs)
              (katldevFor pkgs)
            ];

          shellHook = ''
            export TMPDIR="''${TMPDIR:-/tmp}"
            export KATL_OVMF_CODE="''${KATL_OVMF_CODE:-${pkgs.OVMFFull.fd}/FV/OVMF_CODE.fd}"
            export KATL_OVMF_VARS="''${KATL_OVMF_VARS:-${pkgs.OVMFFull.fd}/FV/OVMF_VARS.fd}"
            export KATL_VMTEST_IMAGE_TOOL="''${KATL_VMTEST_IMAGE_TOOL:-${pkgs.qemu_kvm}/bin/qemu-img}"
            export KATL_VMTEST_VIRSH="''${KATL_VMTEST_VIRSH:-${pkgs.libvirt}/bin/virsh}"
            export KATL_VMTEST_LIBVIRT_URI="''${KATL_VMTEST_LIBVIRT_URI:-qemu:///system}"
            export KATL_VMTEST_LIBVIRT_NETWORK="''${KATL_VMTEST_LIBVIRT_NETWORK:-default}"
            export KATL_VMTEST_LIBVIRT_STORAGE_POOL="''${KATL_VMTEST_LIBVIRT_STORAGE_POOL:-default}"
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
        }
      );
    };
}
