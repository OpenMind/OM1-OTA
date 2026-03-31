import hashlib
import json
import logging
import os
import tempfile
from typing import Optional, Tuple
from urllib.parse import urlparse

import boto3
import requests
import yaml
from botocore.exceptions import BotoCoreError, ClientError

SCHEMA_URL_TEMPLATE = "https://assets.openmind.com/ota/{tag}/schema.json"


class S3FileDownloader:
    """Utility class for downloading files from S3 and verifying checksums."""

    def __init__(self, updates_dir=".ota"):
        self.logger = logging.getLogger(__name__)
        self.updates_dir = os.path.abspath(updates_dir)
        os.makedirs(self.updates_dir, mode=0o755, exist_ok=True)
        try:
            self.s3_client = boto3.client("s3")
        except Exception as e:
            self.logger.warning(f"Failed to initialize boto3 S3 client: {e}")
            self.s3_client = None

    def download_file_from_s3_url(
        self, s3_url: str, local_path: Optional[str] = None
    ) -> Optional[str]:
        """
        Download a file from S3 using either boto3 or direct HTTP request.

        Parameters
        ----------
        s3_url : str
            S3 URL (s3:// or https://)
        local_path : Optional[str]
            Optional local file path. If None, uses temporary file.

        Returns
        -------
        Optional[str]
            Path to downloaded file or None if failed
        """
        try:
            if s3_url.startswith("s3://"):
                return self._download_with_boto3(s3_url, local_path)
            elif s3_url.startswith("https://"):
                return self._download_with_requests(s3_url, local_path)
            else:
                self.logger.error(f"Unsupported S3 URL format: {s3_url}")
                return None
        except Exception as e:
            self.logger.error(f"Failed to download file from {s3_url}: {e}")
            return None

    def _download_with_boto3(
        self, s3_url: str, local_path: Optional[str] = None
    ) -> Optional[str]:
        """
        Download file using boto3 client.

        Parameters
        ----------
        s3_url : str
            S3 URL in the format s3://bucket/key
        local_path : Optional[str]
            Local file path to save the downloaded file. If None, a temporary file is created.

        Returns
        -------
        Optional[str]
            Path to the downloaded file or None if failed
        """
        if not self.s3_client:
            self.logger.error("boto3 S3 client not available")
            return None

        parsed_url = urlparse(s3_url)
        bucket_name = parsed_url.netloc
        object_key = parsed_url.path.lstrip("/")

        if not local_path:
            temp_file = tempfile.NamedTemporaryFile(delete=False, suffix=".yaml")
            local_path = temp_file.name
            temp_file.close()

        try:
            self.logger.info(f"Downloading {bucket_name}/{object_key} to {local_path}")
            self.s3_client.download_file(bucket_name, object_key, local_path)
            return local_path
        except (BotoCoreError, ClientError) as e:
            self.logger.error(f"boto3 download failed: {e}")
            if os.path.exists(local_path):
                os.unlink(local_path)
            return None

    def _download_with_requests(
        self, s3_url: str, local_path: Optional[str] = None
    ) -> Optional[str]:
        """
        Download file using HTTP request (for public S3 URLs or pre-signed URLs).

        Parameters
        ----------
        s3_url : str
            HTTPS S3 URL
        local_path : Optional[str]
            Local file path to save the downloaded file. If None, a temporary file is created.

        Returns
        -------
        Optional[str]
            Path to the downloaded file or None if failed
        """
        if not local_path:
            temp_file = tempfile.NamedTemporaryFile(delete=False, suffix=".yaml")
            local_path = temp_file.name
            temp_file.close()

        try:
            self.logger.info(f"Downloading {s3_url} to {local_path}")
            response = requests.get(s3_url, stream=True, timeout=30)
            response.raise_for_status()

            with open(local_path, "wb") as f:
                for chunk in response.iter_content(chunk_size=8192):
                    f.write(chunk)

            return local_path
        except requests.RequestException as e:
            self.logger.error(f"HTTP download failed: {e}")
            if os.path.exists(local_path):
                os.unlink(local_path)
            return None

    def calculate_file_checksum(
        self, file_path: str, algorithm: str = "sha256"
    ) -> Optional[str]:
        """
        Calculate checksum of a file.

        Parameters
        ----------
        file_path : str
            Path to the file
        algorithm : str
            Hash algorithm to use (e.g., 'sha256', 'md5')

        Returns
        -------
        Optional[str]
            Calculated checksum as a hexadecimal string, or None if failed
        """
        try:
            hash_func = hashlib.new(algorithm)
            with open(file_path, "rb") as f:
                for chunk in iter(lambda: f.read(8192), b""):
                    hash_func.update(chunk)
            return hash_func.hexdigest()
        except Exception as e:
            self.logger.error(f"Failed to calculate checksum for {file_path}: {e}")
            return None

    def verify_checksum(
        self, file_path: str, expected_checksum: str, algorithm: str = "sha256"
    ) -> bool:
        """
        Verify file checksum.

        Parameters
        ----------
        file_path : str
            Path to the file
        expected_checksum : str
            Expected checksum value
        algorithm : str
            Hash algorithm used (e.g., 'sha256', 'md5')

        Returns
        -------
        bool
            True if checksum matches, False otherwise
        """
        actual_checksum = self.calculate_file_checksum(file_path, algorithm)
        if not actual_checksum:
            return False

        match = actual_checksum.lower() == expected_checksum.lower()
        if match:
            self.logger.info(f"Checksum verification passed for {file_path}")
        else:
            self.logger.error(
                f"Checksum verification failed for {file_path}. "
                f"Expected: {expected_checksum}, Actual: {actual_checksum}"
            )
        return match

    def download_and_verify_yaml(
        self, s3_url: str, expected_checksum: str, algorithm: str = "sha256"
    ) -> Tuple[Optional[dict], Optional[str]]:
        """
        Download YAML file from S3, verify checksum, and parse content.

        Parameters
        ----------
        s3_url : str
            S3 URL of the YAML file
        expected_checksum : str
            Expected checksum value
        algorithm : str
            Hash algorithm used for checksum
        """
        local_path = self.download_file_from_s3_url(s3_url)
        if not local_path:
            return None, None

        try:
            if not self.verify_checksum(local_path, expected_checksum, algorithm):
                os.unlink(local_path)
                return None, None

            with open(local_path, "r", encoding="utf-8") as f:
                yaml_content = yaml.safe_load(f)

            self.logger.info(
                f"Successfully downloaded and verified YAML file from {s3_url}"
            )
            return yaml_content, local_path

        except yaml.YAMLError as e:
            self.logger.error(f"Failed to parse YAML file {local_path}: {e}")
            os.unlink(local_path)
            return None, None
        except Exception as e:
            self.logger.error(
                f"Unexpected error processing YAML file {local_path}: {e}"
            )
            os.unlink(local_path)
            return None, None

    def download_schema(self, tag: str) -> Optional[dict]:
        """
        Download schema JSON from S3 and save to local cache.
        """
        schema_url = SCHEMA_URL_TEMPLATE.format(tag=tag)
        cache_path = os.path.join(self.updates_dir, f"{tag}_schema.json")

        local_path = self.download_file_from_s3_url(schema_url, cache_path)
        if not local_path:
            return None

        try:
            with open(cache_path, "r") as f:
                schema = json.load(f)
            self.logger.info(f"Downloaded and cached schema to {cache_path}")
            return schema
        except Exception as e:
            self.logger.error(f"Failed to parse schema: {e}")
            return None

    def get_default_env(self, service_name: str, tag: str) -> dict[str, str]:
        """
        Get default env values from cached schema.
        """
        cache_path = os.path.join(self.updates_dir, f"{tag}_schema.json")
        if not os.path.exists(cache_path):
            return {}

        try:
            with open(cache_path, "r") as f:
                schema = json.load(f)
            env = schema.get(service_name, {}).get("env", {})
            if env:
                self.logger.info(
                    f"Using schema defaults for {service_name}/{tag}: {env}"
                )
            return env
        except Exception as e:
            self.logger.error(f"Failed to read schema cache: {e}")
            return {}

    def get_schema_env_keys(self, tag: str, image_name: str) -> list[str]:
        """
        Get valid env keys for a service by matching image name.
        """
        cache_path = os.path.join(self.updates_dir, f"{tag}_schema.json")
        if not os.path.exists(cache_path):
            return []

        try:
            with open(cache_path, "r") as f:
                schema = json.load(f)

            for service in schema.values():
                if service.get("image") == image_name:
                    return list(service.get("env", {}).keys())
            return []
        except Exception as e:
            self.logger.error(f"Failed to read schema cache: {e}")
            return []
