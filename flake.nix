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
      goPackage = {
        version = "git-${builtins.substring 0 8 revision}";
        src = self;
        vendorHash = "sha256-zlkVLnuWECGs2rgABXLWZN0LRHHKI9Hlb9EJCNhQ6Zc=";
        env.CGO_ENABLED = "0";
      };
      buildKatlGo = pkgs: args: pkgs.buildGoModule (goPackage // args);
      katlctlPackageFor =
        pkgs:
        buildKatlGo pkgs {
          pname = "katlctl";
          subPackages = [ "cmd/katlctl" ];
          nativeBuildInputs = [ pkgs.installShellFiles ];
          ldflags = [
            "-s"
            "-w"
            "-X main.version=git-${builtins.substring 0 8 revision}"
            "-X main.commit=${revision}"
            "-X main.date=${self.lastModifiedDate or "unknown"}"
          ];
          postInstall = ''
            "$out/bin/katlctl" completion bash > katlctl.bash
            "$out/bin/katlctl" completion fish > katlctl.fish
            "$out/bin/katlctl" completion zsh > katlctl.zsh
            installShellCompletion --cmd katlctl \
              --bash katlctl.bash \
              --fish katlctl.fish \
              --zsh katlctl.zsh
          '';
          meta.mainProgram = "katlctl";
        };
      katlQueuePackageFor =
        pkgs:
        let
          runtimePath = nixpkgs.lib.makeBinPath (
            [ pkgs.gh pkgs.git ] ++ nixpkgs.lib.optional pkgs.stdenv.hostPlatform.isLinux pkgs.libnotify
          );
        in
        buildKatlGo pkgs {
          pname = "katl-queue";
          subPackages = [ "cmd/katl-queue" ];
          nativeBuildInputs = [ pkgs.makeBinaryWrapper ];
          nativeCheckInputs = [ pkgs.git ];
          postInstall = ''
            wrapProgram "$out/bin/katl-queue" --prefix PATH : "${runtimePath}"
          '';
          meta.mainProgram = "katl-queue";
        };
      katlDevEvidencePackageFor =
        pkgs:
        buildKatlGo pkgs {
          pname = "katl-dev-evidence";
          env.CGO_ENABLED = "1";
          subPackages = [ "cmd/katl-dev-evidence" ];
          nativeBuildInputs = [ pkgs.makeBinaryWrapper ];
          postInstall = ''
            wrapProgram "$out/bin/katl-dev-evidence" --prefix PATH : "${pkgs.lib.makeBinPath [ pkgs.git ]}"
          '';
          meta.mainProgram = "katl-dev-evidence";
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
      repoScriptFor =
        pkgs: name:
        pkgs.writeShellScriptBin name ''
          repo_root="$(${pkgs.git}/bin/git rev-parse --show-toplevel 2>/dev/null)" || {
            echo "error: ${name} must be run from the Katl checkout" >&2
            exit 1
          }
          exec "$repo_root/scripts/${name}" "$@"
        '';
      gitCommitWrappedFor =
        pkgs:
        pkgs.writeShellApplication {
          name = "git-commit-wrapped";
          runtimeInputs = [
            pkgs.coreutils
            pkgs.git
          ];
          text = builtins.readFile ./scripts/git-commit-wrapped;
        };
      devToolsFor =
        pkgs:
        [
          (katlctlFor pkgs)
          (katldevFor pkgs)
          (katlDevEvidencePackageFor pkgs)
          (gitCommitWrappedFor pkgs)
        ]
        ++ map (repoScriptFor pkgs) [
          "codex-role"
          "vmtest-run"
          "vmtest-clean"
          "vmtest-debug"
          "vmtest-exec"
          "worktree-clean"
          "check-fast"
          "hermetic"
        ];
      # Tools that can change a test's verdict: tests, and the scripts they
      # run, fail or skip without them. CI and the development shell share
      # this list so a test sees the same tool versions in both.
      testToolsFor =
        pkgs: with pkgs; [
          age
          bashInteractive
          bubblewrap
          cacert
          dosfstools
          e2fsprogs
          erofs-utils
          gh
          git
          go
          jq
          kmod
          kubectl
          mtools
          openssh
          rpm
          skopeo
          sops
          squashfsTools
          systemdUkify
          unzip
          util-linux
          xorriso
          zstd
        ];
      testShellHookFor = pkgs: ''
        # Run Go tests through scripts/hermetic -- go test ...
        export KATL_REQUIRE_TOOLS=1
        # Prepare the caches before the wrapper requires them as bind sources.
        if katl_go_caches="$(${pkgs.go}/bin/go env GOCACHE GOMODCACHE)"; then
          mapfile -t katl_go_cache_dirs <<< "$katl_go_caches"
          for katl_go_cache in "''${katl_go_cache_dirs[@]}"; do
            if [[ "$katl_go_cache" == /* ]]; then
              mkdir -p -- "$katl_go_cache"
            fi
          done
        fi
        unset katl_go_caches katl_go_cache_dirs katl_go_cache
        if katl_repo_root="$(${pkgs.git}/bin/git rev-parse --show-toplevel 2>/dev/null)"; then
          source ${./scripts/dev-shell-env} "$katl_repo_root"
        fi
        unset katl_repo_root
      '';
      ciShellFor =
        pkgs:
        pkgs.mkShell {
          packages = testToolsFor pkgs;
          shellHook = testShellHookFor pkgs;
        };
      devShellFor =
        pkgs:
        pkgs.mkShell {
          packages =
            testToolsFor pkgs
            ++ (with pkgs; [
              cpio
              curl
              fish
              gofumpt
              libvirt
              OVMFFull
              podman
              protobuf
              protoc-gen-go
              protoc-gen-go-grpc
              qemu_kvm
              zsh
            ])
            ++ devToolsFor pkgs
            ++ [ (repoScriptFor pkgs "katl-queue") ];

          shellHook = testShellHookFor pkgs + ''
            source ${katlctlPackageFor pkgs}/share/bash-completion/completions/katlctl.bash
            export XDG_DATA_DIRS="${katlctlPackageFor pkgs}/share''${XDG_DATA_DIRS:+:$XDG_DATA_DIRS}"
            # FPATH replaces Zsh's built-in search path, so retain its stock functions.
            export FPATH="${katlctlPackageFor pkgs}/share/zsh/site-functions:''${FPATH:-$(${pkgs.zsh}/bin/zsh -f -c 'print -r -- $FPATH')}"
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
      packages =
        forPackageSystems (
          system:
          let
            pkgs = pkgsFor system;
            katlctl = katlctlPackageFor pkgs;
          in
          {
            inherit katlctl;
            katl-queue = katlQueuePackageFor pkgs;
            katl-dev-evidence = katlDevEvidencePackageFor pkgs;
            default = katlctl;
          }
        )
        // {
          aarch64-linux = {
            katl-queue = katlQueuePackageFor (pkgsFor "aarch64-linux");
            katl-dev-evidence = katlDevEvidencePackageFor (pkgsFor "aarch64-linux");
          };
        };
      homeManagerModules.katl-dev = import ./contrib/home-manager/katl-dev.nix {
        katl = self;
        inherit devToolsFor;
      };
      devShells = forDevSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          ci = ciShellFor pkgs;
          default = devShellFor pkgs;
        }
      );
    };
}
