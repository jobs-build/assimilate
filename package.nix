{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.7.1";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-BK0P/yFvB5vof8tqHVXpDcFWmCBQtJkpFre9dRHu6d0=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
