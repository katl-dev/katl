katl:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.katl-queue-watch;
  package = katl.packages.${pkgs.stdenv.hostPlatform.system}.katl-queue;
  nanoseconds = {
    ms = 1000000;
    s = 1000000000;
    m = 60000000000;
    h = 3600000000000;
  };
  validInterval =
    value:
    let
      parts = builtins.match "([1-9][0-9]{0,12})(ms|s|m|h)" value;
    in
    parts != null
    &&
      builtins.fromJSON (builtins.elemAt parts 0)
      <= builtins.div 9223372036854775807 nanoseconds.${builtins.elemAt parts 1};
in
{
  options.services.katl-queue-watch = {
    enable = lib.mkEnableOption "the Katl merge queue watcher";

    interval = lib.mkOption {
      type = lib.types.addCheck lib.types.str validInterval;
      default = "60s";
      example = "30s";
      description = "Time between GitHub polls, as a positive whole number followed by ms, s, m, or h.";
    };

    notify = lib.mkEnableOption "desktop notifications for Katl watch results";
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ package ];

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
        ExecStart = "${lib.getExe package} watchd --interval ${cfg.interval}${lib.optionalString cfg.notify " --notify"}";
        Environment = lib.optionals cfg.notify [ "PATH=${lib.makeBinPath [ pkgs.libnotify ]}" ];
        UMask = "0077";
        Restart = "on-failure";
      };
      Install.WantedBy = [ "default.target" ];
    };
  };
}
