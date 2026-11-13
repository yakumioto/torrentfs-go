"""Check the public image-publishing contract without contacting GHCR."""

from pathlib import Path
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]


class NightlyPublishTest(unittest.TestCase):
    def test_publish_fixed_and_rolling_tags_for_both_architectures(self):
        workflow = yaml.safe_load(
            (ROOT / ".github/workflows/nightly.yml").read_text(encoding="utf-8")
        )
        publish_inputs = [
            step["with"]
            for job in workflow["jobs"].values()
            for step in job.get("steps", [])
            if step.get("uses", "").startswith("docker/build-push-action@")
            and step.get("with", {}).get("push") is True
        ]
        self.assertEqual(len(publish_inputs), 1, "publish both tags in one build")
        publish = publish_inputs[0]
        self.assertEqual(
            {tag.strip() for tag in publish["tags"].splitlines() if tag.strip()},
            {
                "${{ env.IMAGE_REPOSITORY }}:${{ steps.metadata.outputs.tag }}",
                "${{ env.IMAGE_REPOSITORY }}:nightly",
            },
            "publish the immutable build tag and rolling nightly tag, not latest",
        )
        self.assertEqual(
            {platform.strip() for platform in publish["platforms"].split(",")},
            {"linux/amd64", "linux/arm64"},
        )


if __name__ == "__main__":
    unittest.main()
