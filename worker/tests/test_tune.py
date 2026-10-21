"""sciot-worker tune: machine detection, candidates, load, choice, end to end."""

from __future__ import annotations

import json
import time

import pytest

from sciot_worker import __main__ as cli
from sciot_worker import tune
from sciot_worker.tune import ComboResult, Load

# --- machine ------------------------------------------------------------------------


@pytest.mark.parametrize(
    "cpu_max, cores",
    [
        ("max 100000", None),
        ("200000 100000", 2),
        ("150000 100000", 2),
        ("50000 100000", 1),
        ("", None),
        (None, None),
        ("garbage", None),
    ],
)
def test_cgroup_cpu_limit(cpu_max, cores):
    assert tune.cgroup_cpu_limit(cpu_max) == cores


def test_parse_cpu_list():
    assert tune.parse_cpu_list("0-3,8,10-11") == 7
    assert tune.parse_cpu_list("5") == 1
    assert tune.parse_cpu_list(None) is None
    assert tune.parse_cpu_list("x-y") is None


def test_linux_physical_cores(tmp_path):
    # 2 cores with 2 hyper-threads each: 4 logical CPUs, 2 physical cores.
    for cpu, core in enumerate([0, 0, 1, 1]):
        topo = tmp_path / f"cpu{cpu}" / "topology"
        topo.mkdir(parents=True)
        (topo / "physical_package_id").write_text("0\n")
        (topo / "core_id").write_text(f"{core}\n")
    assert tune.linux_physical_cores(tmp_path) == 2
    assert tune.linux_physical_cores(tmp_path / "missing") is None


def test_detect_machine_on_this_host(tmp_path):
    m = tune.detect_machine(cgroup_root=tmp_path)  # no cpu.max here: no cgroup limit
    assert 1 <= m.usable_cores <= m.logical_cores
    assert m.usable_source in ("cpu_count", "affinity")
    assert "usable" in tune.describe_machine(m)


def test_detect_machine_respects_a_cgroup_limit(tmp_path):
    (tmp_path / "cpu.max").write_text("100000 100000\n")  # one core
    m = tune.detect_machine(cgroup_root=tmp_path)
    assert (m.usable_cores, m.usable_source) == (1, "cgroup")


# --- candidates -----------------------------------------------------------------------


@pytest.mark.parametrize(
    "cores, combos",
    [
        (12, [(12, 1), (6, 2), (4, 3), (3, 4), (2, 6), (1, 12)]),
        (8, [(8, 1), (4, 2), (2, 4), (1, 8)]),
        (7, [(7, 1), (3, 2), (2, 3), (1, 7)]),  # prime: the closest combinations
        (1, [(1, 1)]),
    ],
)
def test_candidate_combos(cores, combos):
    got = tune.candidate_combos(cores)
    assert got == combos
    assert all(s * t <= cores for s, t in got), "never more threads than cores"


def test_parse_combos():
    assert tune.parse_combos("2x6,3X4") == [(2, 6), (3, 4)]
    for bad in ("2*6", "0x4", "x"):
        with pytest.raises(ValueError):
            tune.parse_combos(bad)


# --- load and choice ------------------------------------------------------------------


def test_percentile():
    assert tune.percentile([1, 2, 3, 4], 50) == 2.5
    assert tune.percentile([5], 95) == 5


def test_run_load_runs_concurrent_clients():
    def call():
        time.sleep(0.01)

    single = tune.run_load(call, 1, 0.2)
    double = tune.run_load(call, 2, 0.2)
    assert single.requests > 5 and single.latency_ms_p50 >= 10
    # Two clients waiting in parallel: about twice the requests.
    assert double.throughput_rps > 1.5 * single.throughput_rps


def result(slots, threads, latency, throughput):
    load1 = Load(1, 10, 1.0, 1000 / latency, latency, latency)
    loadn = Load(slots, 10, 1.0, throughput, latency, latency)
    return ComboResult(slots, threads, -1, 1.0, None, load1, loadn)


def test_recommend():
    results = [
        result(1, 12, 28.5, 35),
        result(2, 6, 29.9, 67),
        result(3, 4, 38.4, 78),
        result(12, 1, 116, 102),
    ]
    c = tune.recommend(results)
    assert c["lowest_latency"] == "1x12"
    assert c["highest_throughput"] == "12x1"
    # Within 10 % of 28.5 ms: 1x12 and 2x6; of these 2x6 has more throughput.
    assert c["recommended"] == "2x6"


# --- end to end -------------------------------------------------------------------------


def test_tune_end_to_end(model_dir, tmp_path, capsys):
    out = tmp_path / "tune.json"
    rc = cli.main(
        [
            "tune",
            "--model",
            f"tiny={model_dir}",
            "--combos",
            "1x1,2x1",
            "--seconds",
            "0.3",
            "--warmup",
            "0.1",
            "--splits=-1,4",
            "--out",
            str(out),
        ]
    )
    assert rc == 0
    report = json.loads(out.read_text())
    assert set(report["results"]) == {"-1", "4"}
    first, second = report["results"]["-1"]
    assert (first["label"], second["label"]) == ("1x1", "2x1")
    assert first["single"]["requests"] > 0 and second["full"]["concurrency"] == 2
    assert second["output_max_abs_diff"] == 0.0, "same input, same output on every combination"
    assert report["config_snippet"]["worker"]["slots"] in (1, 2)
    assert report["machine"]["usable_cores"] >= 1
    printed = capsys.readouterr().out
    assert "config.json" in printed and "recommended" in printed


def test_tune_unknown_split(model_dir, tmp_path):
    with pytest.raises(ValueError, match="split point 99"):
        cli.main(
            [
                "tune",
                "--model",
                f"tiny={model_dir}",
                "--combos",
                "1x1",
                "--seconds",
                "0.1",
                "--warmup",
                "0.05",
                "--splits",
                "99",
                "--out",
                str(tmp_path / "t.json"),
            ]
        )
