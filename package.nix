{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.3.0";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-A5Qk7Mwx13A8vFPEIIxgOex1Cd8lEHt1HkIu0R2AyuA=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
