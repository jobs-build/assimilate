{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.8.1";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-gHt1Fm4XwryW499TrA9uu5pGtpMs3NguTNZZsO+OJ9c=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
