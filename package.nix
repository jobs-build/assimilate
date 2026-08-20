{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.3.0";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-QfMQamWctWS4mzbh98lkAhPQ+QYVmXCiVFyJjSyUHh4=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
