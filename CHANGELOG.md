# Changelog

## 0.1.0 (2026-11-14)


### Features

* bootstrap and status with the Bootstrap cluster phase ([#18](https://github.com/klconsultancy/selfhostctl/issues/18)) ([e18d98f](https://github.com/klconsultancy/selfhostctl/commit/e18d98f675920428f83dae80ee5708da1b5dd090))
* bootstrap applies the cluster, waits until Ready, and secures the Recovery bundle ([#22](https://github.com/klconsultancy/selfhostctl/issues/22)) ([4c44b75](https://github.com/klconsultancy/selfhostctl/commit/4c44b752297bfb28102807fe365931c006ad1e12))
* give Flux its git credential and check that it syncs ([#26](https://github.com/klconsultancy/selfhostctl/issues/26)) ([11854dd](https://github.com/klconsultancy/selfhostctl/commit/11854dd1c8f7fc46bae521b28a98b89357f3857f))
* install beforeApply manifests and charts on the Bootstrap cluster ([#21](https://github.com/klconsultancy/selfhostctl/issues/21)) ([5db3b8e](https://github.com/klconsultancy/selfhostctl/commit/5db3b8e3deba2e399d7986b74488f6cc22f4e67f))
* install the declared providers on the Bootstrap cluster ([#20](https://github.com/klconsultancy/selfhostctl/issues/20)) ([d6db2c1](https://github.com/klconsultancy/selfhostctl/commit/d6db2c164f10a324d1035debfc34940405535d0f))
* pivot the Management cluster to manage itself ([#25](https://github.com/klconsultancy/selfhostctl/issues/25)) ([753b1ab](https://github.com/klconsultancy/selfhostctl/commit/753b1ab0ba882fff2ffafcd635e23ecc61292c05))
* recover the rest of the way: new machines, the move back, the finish ([#29](https://github.com/klconsultancy/selfhostctl/issues/29)) ([c04229c](https://github.com/klconsultancy/selfhostctl/commit/c04229c70067261d447e463f88efcf9c6552aee1))
* recover through a Break-glass controller into a Bootstrap cluster ([#27](https://github.com/klconsultancy/selfhostctl/issues/27)) ([bfe3456](https://github.com/klconsultancy/selfhostctl/commit/bfe3456b63302cafe4f7568ff12152c7bb087c91))
* recover with the capmox Infrastructure adapter ([#30](https://github.com/klconsultancy/selfhostctl/issues/30)) ([83d40ca](https://github.com/klconsultancy/selfhostctl/commit/83d40ca0396f2d4d481ef2cbdc5c72e8828892c1))
* upgrade the Management cluster's providers to the config ([#31](https://github.com/klconsultancy/selfhostctl/issues/31)) ([e6f6f50](https://github.com/klconsultancy/selfhostctl/commit/e6f6f5005d03e78e8bd124321b5044c8c0cf04c5))


### Bug Fixes

* **deps:** update module go.opentelemetry.io/otel/trace to v1.47.0 [security] ([#33](https://github.com/klconsultancy/selfhostctl/issues/33)) ([70cc3d1](https://github.com/klconsultancy/selfhostctl/commit/70cc3d1886ef40266e01d3c4a7f9d531d698c02b))
