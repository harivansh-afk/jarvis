{
  description = "Jarvis: a voice interface to ongoing work on Linux";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-linux"
        "x86_64-linux"
      ];
      eachSystem = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = eachSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          jarvis = pkgs.buildGoModule {
            pname = "jarvis";
            version = "0.1.0";
            src = pkgs.lib.cleanSource ./.;
            vendorHash = "sha256-h+X1PCtjeMu6V/19QwwyrNEkFP0d8VboYhEzcmmLzSc=";
            subPackages = [ "cmd/jarvisd" ];
            env.CGO_ENABLED = 1;
            doCheck = true;
            checkPhase = ''
              runHook preCheck
              go test ./...
              runHook postCheck
            '';
            meta = {
              description = "Voice and terminal observation daemon";
              mainProgram = "jarvisd";
              platforms = systems;
            };
          };
        in
        {
          default = jarvis;
          jarvisd = jarvis;
        }
      );

      checks = eachSystem (system: {
        package = self.packages.${system}.default;
      });
      devShells = eachSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gcc
              pkgs.gopls
              pkgs.nixfmt
            ];
          };
        }
      );
      formatter = eachSystem (system: nixpkgs.legacyPackages.${system}.nixfmt);
      nixosModules.default = import ./nix/module.nix self;
    };
}
