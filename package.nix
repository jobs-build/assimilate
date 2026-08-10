{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.2.6";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-4qYCm+5ibUtZM8UaO201uPGFtjzK9tmC2ZeRrSVYFb0=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
