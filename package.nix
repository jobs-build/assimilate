{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.2.7";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-AwBGT+1XZ3IVF7I7cjqfsIFlLK6UdedqMkGy12Dyrbg=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
