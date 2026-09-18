{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.7.4";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-ejqljMVCeQzQJ+4BX+Z/xvPI/NZ0R/vcHiTPw8XPnOs=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
