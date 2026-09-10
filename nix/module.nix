self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.jarvis;
in
{
  options.services.jarvis = {
    enable = lib.mkEnableOption "Jarvis voice and terminal interface";
    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      description = "Jarvis daemon package.";
    };
    muxPackage = lib.mkOption {
      type = lib.types.package;
      description = "Mux package with inspect, observe and input commands.";
    };
    user = lib.mkOption {
      type = lib.types.str;
      description = "User whose terminals Jarvis can access.";
    };
    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:19743";
    };
    origins = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "http://localhost:19743"
        "http://127.0.0.1:19743"
      ];
      description = "Exact allowed browser origins, including scheme and port.";
    };
    stateDirectory = lib.mkOption {
      type = lib.types.str;
      default = "jarvis";
    };
    muxSocket = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
    };
    backendModel = lib.mkOption {
      type = lib.types.str;
      default = "gpt-6-astra";
    };
    reasoningEffort = lib.mkOption {
      type = lib.types.enum [
        "low"
        "medium"
        "high"
        "xhigh"
      ];
      default = "low";
    };
    environmentFiles = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Runtime secret files; OPENAI_API_KEY enables voice. Never put secrets in Nix values.";
    };
    openaiKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
    };
  };
  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = lib.hasPrefix "127.0.0.1:" cfg.listen;
        message = "Jarvis must bind to loopback; expose it through an authenticated TLS proxy or SSH forwarding.";
      }
    ];
    systemd.services.jarvis = {
      description = "Jarvis voice interface to ongoing work";
      wantedBy = [ "multi-user.target" ];
      wants = [ "muxd.service" ];
      after = [
        "network-online.target"
        "muxd.service"
      ];
      environment.HOME = config.users.users.${cfg.user}.home;
      serviceConfig = {
        User = cfg.user;
        StateDirectory = cfg.stateDirectory;
        StateDirectoryMode = "0700";
        UMask = "0077";
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "--listen"
            cfg.listen
            "--state-dir"
            "/var/lib/${cfg.stateDirectory}"
            "--mux"
            "${cfg.muxPackage}/bin/muxd"
            "--origins"
            (lib.concatStringsSep "," cfg.origins)
            "--backend-model"
            cfg.backendModel
            "--reasoning-effort"
            cfg.reasoningEffort
          ]
          ++ lib.optionals (cfg.muxSocket != null) [
            "--mux-socket"
            cfg.muxSocket
          ]
          ++ lib.optionals (cfg.openaiKeyFile != null) [
            "--openai-key-file"
            cfg.openaiKeyFile
          ]
        );
        EnvironmentFile = cfg.environmentFiles;
        Restart = "on-failure";
        RestartSec = "2s";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = "read-only";
        PrivateDevices = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
      };
    };
  };
}
