import ray

@ray.remote(resources={"TPU": 4})
def tpu_cores():
    import jax

    print("TPU cores:" + str(jax.device_count()))
    return "TPU cores:" + str(jax.device_count())


ray.init(
    runtime_env={
        "pip": [
            "jax[tpu]",
            "-f https://storage.googleapis.com/jax-releases/libtpu_releases.html",
            "ml_dtypes==0.2.0"
        ]
    }
)

print(ray.cluster_resources())

num_workers = 4
result = [tpu_cores.remote() for _ in range(num_workers)]
print(ray.get(result))
