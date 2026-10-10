{
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
      go = pkgs.go_1_26;
      buildGoModule = pkgs.buildGoModule.override { inherit go; };
      main = buildGoModule {
        name = "mpris-remote";
        src = ./.;
        vendorHash = "sha256-t+NXeZ0j0imD5VCXeB50lMwv8gOl8bXO3E4s4liNtdE=";
        meta.mainProgram = "mpris-remote";
      };
    in
    {
      packages.x86_64-linux.default = main;
      packages.x86_64-linux.mpris-remote = main;
      devShells.x86_64-linux.default = pkgs.mkShell {
        inputsFrom = [ main ];
        packages = with pkgs; [
          go
          gopls
          gotools
          protobuf
          protoc-gen-go
        ];
      };
    };
}
