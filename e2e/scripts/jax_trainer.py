import os

import jax
import jax.numpy as jnp
import ray
from ray.train.v2.api.config import ScalingConfig
from ray.train.v2.jax import JaxTrainer


def train_loop_per_worker(config):
    devices = jax.devices()
    print(f"Jax devices: {devices}")
    assert len(devices) > 0, "No JAX devices found!"

    # Test basic JAX computation on TPU
    x = jax.random.normal(jax.random.PRNGKey(0), (2048, 2048), dtype=jnp.bfloat16)
    y = jax.random.normal(jax.random.PRNGKey(1), (2048, 2048), dtype=jnp.bfloat16)
    z = jnp.matmul(x, y)
    z.block_until_ready()
    print("JAX computation successful.")


def main():
    ray.init()
    accelerator_type = os.environ.get("ACCELERATOR_TYPE", "TPU-V6E")
    topology = os.environ.get("TOPOLOGY", "4x4")
    num_workers = int(os.environ.get("NUM_WORKERS", "4"))
    resources_per_worker = int(os.environ.get("RESOURCES_PER_WORKER", "4"))

    trainer = JaxTrainer(
        train_loop_per_worker=train_loop_per_worker,
        scaling_config=ScalingConfig(
            use_tpu=True,
            num_workers=num_workers,
            topology=topology,
            accelerator_type=accelerator_type,
            resources_per_worker={"TPU": resources_per_worker},
        ),
    )
    result = trainer.fit()
    if result.error:
        print("Training failed!", result.error)
        exit(1)
    print("Training succeeded!")


if __name__ == "__main__":
    main()
