{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.7.0";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-NGNam9nDUDDhpifKy5n05dvbFQmhnuMFWECQdJtgUKg=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
