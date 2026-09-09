{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.7.4";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-0VhjUb69xq0pgHXcR6EMinCFhRnS1O0rwCed0u53ahc=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
