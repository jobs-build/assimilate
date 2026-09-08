{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.7.2";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-xEznzFvJtjy7G4wL1Uc/uokhkmFlVS/blNtFLsEKQWQ=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
