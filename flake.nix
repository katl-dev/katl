{
  description = "Katl development shell";

  inputs = {
    nixpkgs.url = "nixpkgs";
  };

  outputs =
    { self, nixpkgs, ... }:
    let
      devSystems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      packageSystems = [
        "x86_64-linux"
        "aarch64-darwin"
      ];
      forDevSystems = nixpkgs.lib.genAttrs devSystems;
      forPackageSystems = nixpkgs.lib.genAttrs packageSystems;
      pkgsFor = system: import nixpkgs { inherit system; };
      revision = self.rev or self.dirtyRev or "unknown";
      katlctlPackageFor =
        pkgs:
        pkgs.buildGoModule {
          pname = "katlctl";
          version = "git-${builtins.substring 0 8 revision}";
          src = self;
          vendorHash = "sha256-e2EZlawAo8JWsrRl6cbKM+IY2ipbjzDfiRROrfjZgpI=";
          subPackages = [ "cmd/katlctl" ];
          env.CGO_ENABLED = "0";
          ldflags = [
            "-s"
            "-w"
            "-X main.version=git-${builtins.substring 0 8 revision}"
            "-X main.commit=${revision}"
            "-X main.date=${self.lastModifiedDate or "unknown"}"
          ];
          meta.mainProgram = "katlctl";
        };
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
              gofumpt
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
      packages = forPackageSystems (
        system:
        let
          katlctl = katlctlPackageFor (pkgsFor system);
        in
        {
          inherit katlctl;
          default = katlctl;
        }
      );
      devShells = forDevSystems (
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
