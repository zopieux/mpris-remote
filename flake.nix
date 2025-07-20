{
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
      main = pkgs.buildGoModule {
        name = "mpris-remote";
        src = ./.;
        vendorHash = "sha256-cEvZqEb+urHlDYr5cgLs59vj3ngU0y7Y5XWFv5D/LRk=";
      };
    in
    {
      packages.x86_64-linux.default = main;
      devShells.x86_64-linux.default = pkgs.mkShell {
        inputsFrom = [ main ];
        packages = with pkgs; [
          go
          gopls
          gotools
        ];
      };
    };
}
