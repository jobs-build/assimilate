{ lib, buildGoModule }:

buildGoModule {
  pname = "assimilate";
  version = "0.5.0";

  src = lib.cleanSource ./.;

  vendorHash = "sha256-vW9JGxVBNBnWymyw+BM1+Zi/owpLJrpy64y06ML3aFw=";

  subPackages = [ "cmd/assimilate" ];

  ldflags = [ "-s" "-w" ];

  meta = {
    description = "assimilate";
    homepage = "https://github.com/jobs-build/assimilate";
    license = lib.licenses.agpl3Only;
    mainProgram = "assimilate";
  };
}
