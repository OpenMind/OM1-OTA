import json
import logging
import os
import re
import subprocess
import threading
import time
from typing import Optional

import requests

from ..ota import BaseOTA
from ..ota.file_manager import FileManager
from ..utils.s3_utils import S3FileDownloader

OTA_AGENT_SERVER_URL = os.getenv(
    "OTA_AGENT_SERVER_URL", "wss://api.openmind.com/api/core/ota/agent"
)
DOCKER_CONTAINER_STATUS_URL = os.getenv(
    "DOCKER_STATUS_URL", "https://api.openmind.com/api/core/ota/agent/docker"
)
OM_API_KEY = os.getenv("OM_API_KEY")
OM_API_KEY_ID = os.getenv("OM_API_KEY_ID")

logging.basicConfig(level=logging.INFO)


class AgentOTA(BaseOTA):
    """
    OTA Agent class extending BaseOTA for specific agent functionalities.
    """

    def __init__(self, ota_server_url: str, om_api_key: str, om_api_key_id: str):
        super().__init__(ota_server_url, om_api_key, om_api_key_id)

        if not DOCKER_CONTAINER_STATUS_URL:
            logging.error(
                "DOCKER_CONTAINER_STATUS_URL environment variable must be set"
            )
            raise ValueError(
                "DOCKER_CONTAINER_STATUS_URL environment variable must be set"
            )

        self.container_info_url = f"{DOCKER_CONTAINER_STATUS_URL}/info"
        self.container_status_url = f"{DOCKER_CONTAINER_STATUS_URL}/status"
        self.container_descriptions = {
            "om1": "OM1 main container running the robot OS",
            "om1_sensor": "OM1 sensor processing container handling sensor data",
            "orchestrator": "ROS2 Orchestrator service container managing ROS2 nodes",
            "watchdog": "ROS2 Watchdog service container monitoring system health",
            "zenoh_bridge": "Zenoh Bridge service container serving the communication between OM1 and ROS2",
            "om1_avatar": "OM1 Avatar container managing avatar functionalities",
            "om1_monitor": "OM1 Monitor container for system monitoring",
            "om1_video_processor": "OM1 Video Processor container handling video streams",
            "ota_agent": "OM1 OTA Agent container for over-the-air updates",
            "ota_updater": "OM1 OTA Updater container for managing the OTA Agent updates",
            "qwen30b_quantized": "Qwen 30B quantized model container for local AI processing",
            "kokoro_tts": "Kokoro TTS container for text-to-speech functionalities",
            "riva_speech": "NVIDIA Riva Speech container for advanced speech processing",
            "person_following": "Person Following container for managing person following capabilities",
            "text_embedding": "Text Embedding container for generating text embeddings for AI processing",
        }

        self.container_info_thread: Optional[threading.Thread] = None
        self.container_status_thread: Optional[threading.Thread] = None

        self.start_fetching_container_info()
        self.start_reporting_container_status()

        self.set_ota_process_callback(self._report_status_once)

    def _get_image_sha256(self, image_name: str) -> str:
        """
        Get the actual SHA256 digest of a Docker image.

        Parameters
        ----------
        image_name : str
            The name of the Docker image

        Returns
        -------
        str
            The SHA256 hash of the image, or "unknown" if unable to retrieve
        """
        try:
            cmd = [
                "docker",
                "image",
                "inspect",
                image_name,
                "--format",
                "{{.RepoDigests}}",
            ]
            result = subprocess.run(cmd, capture_output=True, text=True, timeout=10)

            if result.returncode == 0 and result.stdout.strip():
                repo_digests = result.stdout.strip()
                sha256_match = re.search(r"@sha256:([a-f0-9]{64})", repo_digests)
                if sha256_match:
                    return sha256_match.group(1)

            id_cmd = ["docker", "image", "inspect", image_name, "--format", "{{.Id}}"]
            id_result = subprocess.run(
                id_cmd, capture_output=True, text=True, timeout=10
            )

            if id_result.returncode == 0 and id_result.stdout.strip():
                image_id = id_result.stdout.strip()
                if image_id.startswith("sha256:"):
                    return image_id[7:]
                return image_id

            return "unknown"

        except (subprocess.TimeoutExpired, FileNotFoundError, Exception) as e:
            logging.warning(f"Failed to get SHA256 for image {image_name}: {e}")
            return "unknown"

    def _filter_env_by_schema(
        self, env_dict: dict[str, str], image_name: str
    ) -> dict[str, str]:
        """
        Filter environment variables by schema keys.
        """
        tag = image_name.split(":")[-1] if ":" in image_name else "latest"

        s3_downloader = S3FileDownloader()
        schema_env_keys = s3_downloader.get_schema_env_keys(tag, image_name)

        result: dict[str, str] = {}
        for key, value in env_dict.items():
            if key in schema_env_keys:
                result[key] = value
        return result

    def _get_container_env_vars(
        self, container_name: str, image_name: str
    ) -> dict[str, str]:
        """
        Get environment variables by combining docker inspect and .env file.

        Parameters
        ----------
        container_name : str
            The name of the Docker container
        image_name : str
            The name of the Docker image (used to extract tag)

        Returns
        -------
        dict[str, str]
            Combined environment variables
        """
        tag = image_name.split(":")[-1] if ":" in image_name else "latest"

        container_env: dict[str, str] = {}
        try:
            cmd = [
                "docker",
                "inspect",
                container_name,
                "--format",
                "{{json .Config.Env}}",
            ]
            result = subprocess.run(cmd, capture_output=True, text=True, timeout=10)
            if result.returncode == 0 and result.stdout.strip():
                env_list = json.loads(result.stdout.strip())
                if isinstance(env_list, list):
                    for item in env_list:
                        if "=" in item:
                            k, v = item.split("=", 1)
                            container_env[k] = v
        except (
            subprocess.TimeoutExpired,
            FileNotFoundError,
            json.JSONDecodeError,
        ) as e:
            logging.warning(
                f"Failed to get env vars via docker inspect for {container_name}: {e}"
            )

        container_env = self._filter_env_by_schema(container_env, image_name)

        file_manager = FileManager()
        file_env = file_manager.read_env_file(container_name, tag)

        result = {**file_env, **container_env}
        return result

    def _fetch_docker_info(self):
        """
        Fetch the list of Docker container info from the server.
        """
        try:
            response = requests.get(
                self.container_info_url,
                headers={"x-api-key": self.om_api_key},
                timeout=10,
            )
            response.raise_for_status()
            data = response.json()
            container_info = data.get("container_info", [])
            if container_info:
                self.container_descriptions = container_info
            time.sleep(30)
        except Exception as e:
            logging.error(f"Failed to fetch Docker container info: {e}")
            time.sleep(5)

    def start_fetching_container_info(self):
        """
        Start the background thread for periodic Docker container info fetching.
        """
        if (
            self.container_info_thread is None
            or not self.container_info_thread.is_alive()
        ):
            self.container_info_thread = threading.Thread(
                target=self._fetch_docker_info, daemon=True
            )
            self.container_info_thread.start()
            logging.info(
                "Started periodic Docker container info fetching (every 30 seconds)"
            )
            return

        logging.info("Docker container info fetching thread is already running.")

    def read_container_status(self) -> Optional[dict]:
        """
        Read the status of Docker containers from the local Docker daemon.

        Returns
        -------
        Optional[dict]
            Dictionary with container statuses or None if failed
        """
        try:
            cmd = ["docker", "ps", "-a", "--format", "json"]
            result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)

            if result.returncode != 0:
                logging.error(f"Docker command failed: {result.stderr}")
                return None

            container_status = {}
            found_containers = set()

            for line in result.stdout.strip().split("\n"):
                if line:
                    try:
                        container_info = json.loads(line)
                        container_name = container_info.get("Names", "").strip()

                        if container_name in self.container_descriptions.keys():
                            found_containers.add(container_name)

                            image_name = container_info.get("Image", "unknown")
                            image_sha256 = self._get_image_sha256(image_name)
                            env_vars = self._get_container_env_vars(
                                container_name, image_name
                            )
                            container_status[container_name] = {
                                "description": self.container_descriptions.get(
                                    container_name, "No description available"
                                ),
                                "state": container_info.get("State", "unknown"),
                                "status": container_info.get("Status", "unknown"),
                                "image": image_name,
                                "image_sha256": image_sha256,
                                "ports": container_info.get("Ports", ""),
                                "created": container_info.get("CreatedAt", ""),
                                "command": container_info.get("Command", ""),
                                "id": container_info.get("ID", ""),
                                "present": True,
                                "env_variables": env_vars,
                            }
                    except json.JSONDecodeError as e:
                        logging.warning(f"Failed to parse container info: {e}")
                        continue

            missing_containers = (
                set(self.container_descriptions.keys()) - found_containers
            )
            for missing_container in missing_containers:
                container_status[missing_container] = {
                    "description": self.container_descriptions.get(
                        missing_container, "No description available"
                    ),
                    "state": "missing",
                    "status": "missing",
                    "image": "unknown",
                    "image_sha256": "unknown",
                    "ports": "",
                    "created": "",
                    "command": "",
                    "id": "",
                    "present": False,
                    "env_variables": self._get_container_env_vars(
                        missing_container, "latest"
                    ),
                }
                logging.warning(
                    f"Container '{missing_container}' is missing from local Docker"
                )

            if missing_containers:
                logging.warning(f"Missing containers: {', '.join(missing_containers)}")

            logging.info(
                f"Successfully fetched status for {len(found_containers)} local containers, {len(missing_containers)} missing"
            )
            return container_status

        except subprocess.TimeoutExpired:
            logging.error("Docker command timed out")
            return None
        except FileNotFoundError:
            logging.error(
                "Docker command not found. Make sure Docker is installed and in PATH"
            )
            return None
        except Exception as e:
            logging.error(f"Failed to read local Docker container status: {e}")
            return None

    def _send_status_to_server(self, status: dict, context: str = ""):
        """
        Send container status to the server.

        Parameters
        ----------
        status : dict
            Container status dictionary
        context : str
            Context string for logging (e.g., "OTA callback", "periodic")
        """
        try:
            response = requests.post(
                self.container_status_url,
                headers={
                    "x-api-key": self.om_api_key,
                    "Content-Type": "application/json",
                },
                json={"container_status": status},
                timeout=10,
            )
            response.raise_for_status()
            context_msg = f" ({context})" if context else ""
            logging.info(
                f"Successfully reported Docker container status to server{context_msg}"
            )
        except Exception as e:
            context_msg = f" ({context})" if context else ""
            logging.error(f"Failed to report Docker container status{context_msg}: {e}")

    def _report_status_once(self, message=None):
        """
        Report the Docker container status to the server once (for OTA callback use).

        Parameters
        ----------
        message : str, optional
            OTA message (unused in this method, but required for callback compatibility)
        """
        status = self.read_container_status()
        if status is not None:
            self._send_status_to_server(status, "OTA callback")

    def _report_status_periodically(self):
        """
        Report the Docker container status to the server periodically (for background thread use only).
        """
        while True:
            status = self.read_container_status()
            if status is not None:
                self._send_status_to_server(status, "periodic")
            time.sleep(5)

    def start_reporting_container_status(self):
        """
        Start the background thread for periodic Docker container status reporting.
        """
        if (
            self.container_status_thread is None
            or not self.container_status_thread.is_alive()
        ):
            self.container_status_thread = threading.Thread(
                target=self._report_status_periodically, daemon=True
            )
            self.container_status_thread.start()
            logging.info(
                "Started periodic Docker container status reporting (every 5 seconds)"
            )
            return

        logging.info("Docker container status reporting thread is already running.")


def main():
    """
    Main function to run the OTA updater WebSocket client.
    """
    try:
        if not OM_API_KEY or not OM_API_KEY_ID:
            logging.error(
                "OM_API_KEY and OM_API_KEY_ID environment variables must be set"
            )
            exit(1)

        ota = AgentOTA(
            ota_server_url=OTA_AGENT_SERVER_URL,
            om_api_key=OM_API_KEY,
            om_api_key_id=OM_API_KEY_ID,
        )

        def callback_with_client(message):
            return ota.ota_process(message, ota.ws_client)

        ota.ws_client.register_message_callback(callback_with_client)
        ota.ws_client.start()

        while True:
            time.sleep(5)

    except Exception as e:
        logging.error(f"Failed to connect to OTA server: {e}")


if __name__ == "__main__":
    main()
