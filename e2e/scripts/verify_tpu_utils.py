import logging
import math
import os
import sys
from typing import Any, Dict, List, Tuple

import jax
import jax.numpy as jnp
import ray
import ray.util.tpu as ray_tpu
from ray.util.scheduling_strategies import PlacementGroupSchedulingStrategy


def setup_tpu_logging(logger_name: str) -> logging.Logger:
    """Configures standard logging to stdout with structured formatting for E2E TPU workloads."""
    logging.basicConfig(
        level=logging.INFO,
        format="%(levelname)s: %(message)s",
        handlers=[logging.StreamHandler(sys.stdout)],
    )
    return logging.getLogger(logger_name)


NUM_WORKERS_MULTIPLIER = 4

logger = setup_tpu_logging("verify_tpu_utils")


def discover_tpu_topology_info(
    nodes: List[Dict[str, Any]],
) -> Tuple[List[Dict[str, Any]], Dict[str, Any], str, str]:
    """Discovers and parses active TPU node metadata from Ray GCS nodes database."""
    tpu_nodes = [
        n for n in nodes if n.get("Alive") and n.get("Resources", {}).get("TPU", 0) > 0
    ]
    if not tpu_nodes:
        raise RuntimeError("No active TPU nodes discovered in the Ray cluster.")

    first_node = tpu_nodes[0]
    labels = first_node.get("Labels", {})
    topology = labels.get("ray.io/tpu-topology")
    accelerator_type = labels.get("ray.io/accelerator-type")

    if not topology or not accelerator_type:
        raise RuntimeError(
            f"Missing required TPU GCS topology labels in node: {first_node}"
        )

    return tpu_nodes, first_node, topology, accelerator_type


# Define remote task to run JAX on physical worker nodes
@ray.remote(num_cpus=0)
def verify_tpu_hardware_and_env(
    expected_global_devices,
    expected_local_devices,
    expected_worker_count,
    expected_chips_per_node,
):
    """Verifies GKE TPU environment values, JAX distribution mesh init, and runs math on TPUs."""
    required_envs = [
        "TPU_NAME",
        "TPU_WORKER_ID",
    ]
    env_values = {}
    for env in required_envs:
        val = os.environ.get(env)
        assert val is not None, f"Missing required env: {env} on TPU worker"
        env_values[env] = val

    # Verify JAX Distributed Initialization and XLA Matrix Multiplication on
    # physical TPU
    jax.distributed.initialize()
    device_count = jax.device_count()
    local_device_count = jax.local_device_count()
    assert device_count == expected_global_devices, (
        f"Expected {expected_global_devices} global JAX devices, " f"got {device_count}"
    )
    assert local_device_count == expected_local_devices, (
        f"Expected {expected_local_devices} local JAX devices, "
        f"got {local_device_count}"
    )

    # Perform math operation on physical TPU to verify XLA JIT & PJRT pipeline
    # works
    x = jax.random.normal(jax.random.PRNGKey(0), (2048, 2048), dtype=jnp.bfloat16)
    y = jax.random.normal(jax.random.PRNGKey(1), (2048, 2048), dtype=jnp.bfloat16)
    z = jnp.matmul(x, y)
    z.block_until_ready()

    # Verify ray.util.tpu getters inside the TPU node container
    pod_name = ray_tpu.get_current_pod_name()
    worker_count = ray_tpu.get_current_pod_worker_count()
    num_chips = ray_tpu.get_num_tpu_chips_on_node()
    assert pod_name is not None, "get_current_pod_name returned None inside TPU Pod"
    assert (
        worker_count == expected_worker_count
    ), f"Expected {expected_worker_count} workers in pod, got {worker_count}"
    assert (
        num_chips == expected_chips_per_node
    ), f"Expected {expected_chips_per_node} TPU chips per node, got {num_chips}"

    return pod_name, env_values


def main():
    logger.info("1. Initializing Ray Client...")
    ray.init()

    logger.info("Discovering cluster topology dynamically from Ray...")
    nodes = ray.nodes()
    logger.info("  Ray Nodes GCS database: %s", nodes)

    (
        tpu_nodes,
        first_node,
        topology,
        accelerator_type,
    ) = discover_tpu_topology_info(nodes)

    # 1. Test get_tpu_version_from_type utility
    tpu_version = ray_tpu.get_tpu_version_from_type(accelerator_type)
    logger.info("Discovered Topology: %s", topology)
    logger.info(
        "Discovered Accelerator Type: %s (clean version: %s)",
        accelerator_type,
        tpu_version,
    )

    # Extract chips capacity from node resources
    expected_chips_per_node = int(first_node.get("Resources", {}).get("TPU", 4))

    # 2. Test get_tpu_worker_resources calculator passing GKE chips override
    hosts_per_slice, _ = ray_tpu.get_tpu_worker_resources(
        topology=topology,
        accelerator_type=tpu_version,
        chips_per_vm=expected_chips_per_node,
    )
    expected_worker_count = len(tpu_nodes)
    expected_global_devices = expected_worker_count * expected_chips_per_node
    expected_local_devices = expected_chips_per_node

    logger.info("Discovered Chips Per Node: %s", expected_chips_per_node)
    logger.info("Discovered Worker Count: %s", expected_worker_count)
    logger.info("Discovered Hosts Per Slice: %s", hosts_per_slice)

    # 3. Test node helper utilities
    first_slice_name = ray_tpu.get_tpu_slice_name_from_node(first_node)
    assert (
        first_slice_name is not None
    ), "get_tpu_slice_name_from_node returned None for active TPU node"
    slice_nodes = ray_tpu.get_tpu_nodes_for_slice(first_slice_name, nodes=nodes)
    assert (
        len(slice_nodes) == hosts_per_slice
    ), f"Expected {hosts_per_slice} slice nodes, got {len(slice_nodes)}"
    logger.info(
        "  Verified TPU slice name: %s containing nodes: %s",
        first_slice_name,
        [n["NodeName"] for n in slice_nodes],
    )

    # 4. Test coordinator environment variable builder
    coordinator_envs = ray_tpu.get_tpu_coordinator_env_vars(
        coordinator_address="10.128.0.2",
        num_slices=2,
        slice_id=1,
        coordinator_port="9090",
    )
    assert coordinator_envs["MEGASCALE_COORDINATOR_ADDRESS"] == "10.128.0.2"
    assert coordinator_envs["MEGASCALE_PORT"] == "9090"
    assert coordinator_envs["MEGASCALE_NUM_SLICES"] == "2"
    assert coordinator_envs["MEGASCALE_SLICE_ID"] == "1"
    logger.info("  Verified coordinator environment variable generator.")

    logger.info("2. Verifying ray.util.tpu query functions...")
    ready_slices = ray_tpu.get_num_ready_tpu_slices(
        topology=topology, accelerator_type=tpu_version
    )
    total_slices = ray_tpu.get_num_tpu_slices(
        topology=topology, accelerator_type=tpu_version
    )
    logger.info("  Ready Slices (Idle): %s", ready_slices)
    logger.info("  Total Slices (Intact): %s", total_slices)
    assert total_slices >= 1, f"Expected at least 1 intact slice, got {total_slices}"

    # Test helper calculators
    num_slices_calc = ray_tpu.get_tpu_num_slices_for_workers(
        topology=topology,
        accelerator_type=tpu_version,
        num_workers=NUM_WORKERS_MULTIPLIER,
    )
    expected_slices = math.ceil(NUM_WORKERS_MULTIPLIER / hosts_per_slice)
    logger.info(
        "  Calculated slices required for %s workers: %s (expected: %s)",
        NUM_WORKERS_MULTIPLIER,
        num_slices_calc,
        expected_slices,
    )
    assert num_slices_calc in (expected_slices, NUM_WORKERS_MULTIPLIER), (
        f"Unexpected slices count: {num_slices_calc}, "
        f"expected {expected_slices} or fallback {NUM_WORKERS_MULTIPLIER}"
    )

    logger.info("3. Reserving SlicePlacementGroup dynamically...")
    # Use the slice_placement_group API to provision the TPU slice
    pg_handle = ray_tpu.slice_placement_group(
        topology=topology,
        accelerator_version=tpu_version,
        chips_per_vm=expected_chips_per_node,
    )
    slice_pg = pg_handle.placement_group

    logger.info("  Waiting for slice placement group to become ready...")
    ray.get(slice_pg.ready(), timeout=30)
    logger.info("  SlicePlacementGroup is ready.")

    # Dispatch validation tasks concurrently to each worker host in the
    # placement group
    tasks = [
        verify_tpu_hardware_and_env.options(
            resources={"TPU": expected_chips_per_node},
            scheduling_strategy=PlacementGroupSchedulingStrategy(
                placement_group=slice_pg, placement_group_bundle_index=i
            ),
        ).remote(
            expected_global_devices,
            expected_local_devices,
            expected_worker_count,
            expected_chips_per_node,
        )
        for i in range(pg_handle.num_hosts)
    ]
    results = ray.get(tasks)
    logger.info(
        "  Scheduled tasks returned results from physical TPU worker hosts: %s",
        results,
    )

    for name, envs in results:
        logger.info("  Verified Host: %s", name)
        for k, v in envs.items():
            logger.info("    %s = %s", k, v)

    logger.info("4. Releasing head reservation placement groups...")
    pg_handle.release_head_pgs()
    logger.info("  Head placement groups released cleanly.")

    logger.info("5. Shutting down worker placement group...")
    pg_handle.shutdown()
    logger.info("  Worker placement group released cleanly.")

    logger.info(
        "All Ray core TPU utilities and JAX/XLA distributed inits verified"
        " successfully."
    )


if __name__ == "__main__":
    main()
