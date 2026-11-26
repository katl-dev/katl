{ katl, devToolsFor }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.programs.katl-dev;
  queue = katl.packages.${pkgs.stdenv.hostPlatform.system}.katl-queue;
  # cgroupfs places rootless Podman's cgroups for containers and VM worlds
  # beneath the delegated scope so the development slice accounts for them.
  containersConf = pkgs.writeText "katl-dev-containers.conf" ''
    [engine]
    cgroup_manager = "cgroupfs"
  '';
  runner = pkgs.writeShellApplication {
    name = "katl-dev-run";
    runtimeInputs = [ pkgs.systemd ];
    text = ''
      if (($# == 0)); then
        printf 'usage: katl-dev-run CMD [ARG...]\n' >&2
        exit 2
      fi
      export CONTAINERS_CONF_OVERRIDE=${containersConf}
      exec systemd-run --user --scope --quiet --collect --slice=katl-dev.slice \
        --property=Delegate=yes --expand-environment=no -- "$@"
    '';
  };
in
{
  options.programs.katl-dev = {
    enable = lib.mkEnableOption "Katl development tools";

    watcher = {
      enable = lib.mkEnableOption "the Katl merge queue watcher";

      interval = lib.mkOption {
        type = lib.types.strMatching "[1-9][0-9]*(ms|s|m|h)";
        default = "60s";
        example = "30s";
        description = "Time between GitHub polls, as a positive whole number followed by ms, s, m, or h.";
      };

      notify = lib.mkEnableOption "desktop notifications for Katl watch results";
    };

    runner.enable = lib.mkEnableOption "the Katl delegated development scope";
  };

  config = lib.mkMerge [
    (lib.mkIf (cfg.enable || cfg.watcher.enable) {
      home.packages = [ queue ];
    })
    (lib.mkIf cfg.enable {
      home.packages = devToolsFor pkgs;
    })
    (lib.mkIf cfg.watcher.enable {
      systemd.user.sockets.katl-queue-watch = {
        Unit.Description = "Katl GitHub watcher socket";
        Socket = {
          ListenStream = "%t/katl-queue/watch.sock";
          DirectoryMode = "0700";
          SocketMode = "0600";
          RemoveOnStop = true;
        };
        Install.WantedBy = [ "sockets.target" ];
      };

      systemd.user.services.katl-queue-watch = {
        Unit = {
          Description = "Katl GitHub watcher";
          Requires = [ "katl-queue-watch.socket" ];
          After = [ "katl-queue-watch.socket" ];
        };
        Service = {
          ExecStart = "${lib.getExe queue} watchd --interval ${cfg.watcher.interval}${lib.optionalString cfg.watcher.notify " --notify"}";
          Environment = lib.optionals cfg.watcher.notify [ "PATH=${lib.makeBinPath [ pkgs.libnotify ]}" ];
          UMask = "0077";
          Restart = "on-failure";
        };
        Install.WantedBy = [ "default.target" ];
      };
    })
    (lib.mkIf cfg.runner.enable {
      home.packages = [ runner ];
      systemd.user.slices.katl-dev.Unit.Description = "Katl development";
    })
  ];
}
