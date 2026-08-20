{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.4.0";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-JKYBSThge4+5vL4HUUj88nrT40O0L+gxWzYUpdfV3X0=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
