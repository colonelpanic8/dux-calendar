{
  config,
  pkgs,
  lib,
  ...
}@args:
let
  cfg = config.programs.dank-calendar;
  jsonFormat = pkgs.formats.json { };
  externalAccountsFile = jsonFormat.generate "external-accounts.json" {
    accounts = lib.mapAttrsToList (name: acc: {
      inherit name;
      inherit (acc) displayName command env;
    }) cfg.externalAccounts;
  };
in
{
  imports = [ (import ./options.nix args) ];

  options.programs.dank-calendar.settings = lib.mkOption {
    type = jsonFormat.type;
    default = { };
    description = "Dux Calendar UI settings written to ~/.config/dankcal/ui-settings.json.";
    example = lib.literalExpression ''
      {
        remindersEnabled = true;
        use24HourClock = true;
        defaultReminderMinutes = 10;
        snoozeMinutes = 5;
      }
    '';
  };

  options.programs.dank-calendar.externalAccounts = lib.mkOption {
    type = lib.types.attrsOf (
      lib.types.submodule (
        { name, ... }:
        {
          options = {
            displayName = lib.mkOption {
              type = lib.types.str;
              default = name;
              description = "Account name shown in the UI.";
            };
            command = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              description = "Provider program and arguments (see docs/external-providers.md).";
            };
            env = lib.mkOption {
              type = lib.types.attrsOf lib.types.str;
              default = { };
              description = "Extra environment for the provider. Do not put secrets here; pass secret file paths instead.";
            };
          };
        }
      )
    );
    default = { };
    description = "External provider accounts managed declaratively; the daemon adds, updates, and removes them to match.";
  };

  config = lib.mkIf cfg.enable {
    programs.dank-calendar.systemd.target = lib.mkDefault config.wayland.systemd.target;

    home.packages = [
      cfg.package
      cfg.quickshell.package
    ];

    systemd.user.services.dcal = lib.mkIf cfg.systemd.enable {
      Unit = {
        Description = "Dux Calendar";
        PartOf = [ cfg.systemd.target ];
        After = [ cfg.systemd.target ];
        X-Restart-Triggers = lib.mkIf (cfg.externalAccounts != { }) [ externalAccountsFile ];
        # With RestartSec=2 the default limit (5 starts per 10s) is never reached, so a persistent failure would restart forever.
        StartLimitIntervalSec = "60";
        StartLimitBurst = "5";
      };

      Service = {
        ExecStart = lib.getExe cfg.package + " run --session --hidden";
        Restart = "on-failure";
        RestartSec = "2";
        Slice = "app.slice";
      };

      Install.WantedBy = [ cfg.systemd.target ];
    };

    xdg.configFile."dankcal/external-accounts.json" = lib.mkIf (cfg.externalAccounts != { }) {
      source = externalAccountsFile;
    };

    xdg.configFile."dankcal/ui-settings.json" = lib.mkIf (cfg.settings != { }) {
      source = jsonFormat.generate "ui-settings.json" cfg.settings;
    };
  };
}
