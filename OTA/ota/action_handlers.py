import logging

from ..utils.s3_utils import S3FileDownloader
from .docker_operations import DockerManager
from .ecr_handler import ECRHandler
from .file_manager import FileManager
from .progress_reporter import ProgressReporter

logging.basicConfig(level=logging.INFO)


class ActionHandlers:
    """Handles different OTA action types (upgrade, start, stop)."""

    def __init__(
        self,
        docker_manager: DockerManager,
        progress_reporter: ProgressReporter,
        file_manager: FileManager,
    ):
        """
        Initialize ActionHandlers.

        Parameters
        ----------
        docker_manager : DockerManager
            Manager for Docker operations
        progress_reporter : ProgressReporter
            Reporter for progress updates
        file_manager : FileManager
            Manager for file operations
        """
        self.docker_manager = docker_manager
        self.progress_reporter = progress_reporter
        self.file_manager = file_manager
        self.ecr_handler = ECRHandler(docker_manager, progress_reporter)

    def handle_upgrade_action(self, data: dict, service_name: str = ""):
        """
        Handle upgrade action for OTA updates.

        Parameters
        ----------
        data : dict
            The parsed message data containing upgrade details
        service_name : str
            The name of the service to upgrade
        """
        tag = data.get("tag")
        s3_url = data.get("s3_url")
        checksum = data.get("checksum")

        if not tag or not s3_url or not checksum:
            logging.error(
                "Invalid upgrade message: missing required fields (tag, s3_url, checksum)"
            )
            self.progress_reporter.send_progress_update(
                "error", "Missing required fields for upgrade action", 0
            )
            return

        logging.info(
            f"OTA Upgrade Details - Tag: {tag}, S3 URL: {s3_url}, Checksum: {checksum}, Service Name: {service_name}"
        )

        s3_downloader = S3FileDownloader()
        yaml_content, local_file_path = s3_downloader.download_and_verify_yaml(
            s3_url=s3_url, expected_checksum=checksum, algorithm="sha256"
        )

        if yaml_content and local_file_path:
            logging.info(
                f"Successfully downloaded and verified YAML file: {local_file_path}"
            )
            logging.info(f"YAML content: {yaml_content}")

            s3_downloader.download_schema(tag)
            env_variables = data.get("env_variables")
            if env_variables is None:
                env_variables = s3_downloader.get_default_env(service_name, tag)
            self.file_manager.update_env_file(service_name, tag, env_variables)

            self.apply_ota_update(service_name, yaml_content, local_file_path, tag)

            self.file_manager.cleanup_temp_file(local_file_path)
        else:
            logging.error("Failed to download or verify YAML file from S3")
            self.progress_reporter.send_progress_update(
                "download_error",
                "Failed to download or verify YAML file from S3",
                0,
            )

    def handle_stop_action(self, data: dict, service_name: str = ""):
        """
        Handle stop action for OTA operations.

        Parameters
        ----------
        data : dict
            The parsed message data containing stop details
        service_name : str
            The name of the service to stop
        """
        logging.info(f"Stopping service: {service_name}")
        self.progress_reporter.send_progress_update(
            "stopping", f"Stopping service {service_name}", 10
        )

        try:
            services_config = {
                "services": {
                    service_name: {
                        "container_name": data.get("container_name", service_name)
                    }
                }
            }

            stop_result = self.docker_manager.stop_docker_services(services_config)

            if stop_result.get("success"):
                logging.info(f"Successfully stopped service: {service_name}")
                self.progress_reporter.send_progress_update(
                    "completed", f"Successfully stopped service {service_name}", 100
                )
            else:
                error_msg = f"Failed to stop service {service_name}: {stop_result.get('error', 'Unknown error')}"
                logging.error(error_msg)
                self.progress_reporter.send_progress_update("error", error_msg, 10)

        except Exception as e:
            error_msg = f"Error stopping service {service_name}: {e}"
            logging.error(error_msg)
            self.progress_reporter.send_progress_update("error", error_msg, 10)

    def handle_start_action(self, data: dict, service_name: str = ""):
        """
        Handle start action for OTA operations.

        Parameters
        ----------
        data : dict
            The parsed message data containing start details
        service_name : str
            The name of the service to start
        """
        logging.info(f"Starting service: {service_name}")
        self.progress_reporter.send_progress_update(
            "starting", f"Starting service {service_name}", 10
        )

        try:
            yaml_content = data.get("yaml_content")

            if not yaml_content:
                config_result = self.file_manager.load_latest_config(service_name)

                if config_result.get("success"):
                    yaml_content = config_result.get("yaml_content")
                    logging.info(
                        f"Loaded latest configuration from: {config_result.get('file_path')}"
                    )
                else:
                    error_msg = f"No YAML content provided and no stored configuration found for service {service_name}"
                    logging.error(error_msg)
                    self.progress_reporter.send_progress_update("error", error_msg, 10)
                    return

            tag = self._extract_tag_from_yaml(yaml_content)  # type: ignore
            s3_downloader = S3FileDownloader()
            s3_downloader.download_schema(tag)
            env_variables = data.get("env_variables")
            if env_variables is None:
                env_variables = s3_downloader.get_default_env(service_name, tag)
            self.file_manager.update_env_file(service_name, tag, env_variables)

            ecr_image = self.ecr_handler.check_image_privacy(yaml_content)
            if ecr_image and not self.ecr_handler.login_with_credentials(ecr_image):
                return

            start_result = self.docker_manager.start_docker_services(yaml_content)  # type: ignore

            if start_result.get("success"):
                logging.info(f"Successfully started service: {service_name}")
                self.progress_reporter.send_progress_update(
                    "completed", f"Successfully started service {service_name}", 100
                )
            else:
                error_msg = f"Failed to start service {service_name}: {start_result.get('error', 'Unknown error')}"
                logging.error(error_msg)
                self.progress_reporter.send_progress_update("error", error_msg, 80)

        except Exception as e:
            error_msg = f"Error starting service {service_name}: {e}"
            logging.error(error_msg)
            self.progress_reporter.send_progress_update("error", error_msg, 10)

    def handle_pause_action(self, data: dict, service_name: str = ""):
        """
        Handle pause action for OTA operations.

        Parameters
        ----------
        data : dict
            The parsed message data containing pause details
        service_name : str
            The name of the service to pause
        """
        logging.info(f"Pausing service: {service_name}")
        self.progress_reporter.send_progress_update(
            "pausing", f"Pausing service {service_name}", 10
        )

        try:
            yaml_content = data.get("yaml_content")

            if not yaml_content:
                config_result = self.file_manager.load_latest_config(service_name)

                if config_result.get("success"):
                    yaml_content = config_result.get("yaml_content")
                    logging.info(
                        f"Loaded latest configuration from: {config_result.get('file_path')}"
                    )
                else:
                    error_msg = f"No YAML content provided and no stored configuration found for service {service_name}"
                    logging.error(error_msg)
                    self.progress_reporter.send_progress_update("error", error_msg, 10)
                    return

            pause_result = self.docker_manager.pause_docker_services(yaml_content)  # type: ignore

            if pause_result.get("success"):
                logging.info(f"Successfully paused service: {service_name}")
                self.progress_reporter.send_progress_update(
                    "completed", f"Successfully paused service {service_name}", 100
                )
            else:
                error_msg = f"Failed to pause service {service_name}: {pause_result.get('error', 'Unknown error')}"
                logging.error(error_msg)
                self.progress_reporter.send_progress_update("error", error_msg, 10)

        except Exception as e:
            error_msg = f"Error pausing service {service_name}: {e}"
            logging.error(error_msg)
            self.progress_reporter.send_progress_update("error", error_msg, 10)

    def handle_unpause_action(self, data: dict, service_name: str = ""):
        """
        Handle unpause action for OTA operations.

        Parameters
        ----------
        data : dict
            The parsed message data containing unpause details
        service_name : str
            The name of the service to unpause
        """
        logging.info(f"Unpausing service: {service_name}")
        self.progress_reporter.send_progress_update(
            "unpausing", f"Unpausing service {service_name}", 10
        )

        try:
            yaml_content = data.get("yaml_content")

            if not yaml_content:
                config_result = self.file_manager.load_latest_config(service_name)

                if config_result.get("success"):
                    yaml_content = config_result.get("yaml_content")
                    logging.info(
                        f"Loaded latest configuration from: {config_result.get('file_path')}"
                    )
                else:
                    error_msg = f"No YAML content provided and no stored configuration found for service {service_name}"
                    logging.error(error_msg)
                    self.progress_reporter.send_progress_update("error", error_msg, 10)
                    return

            unpause_result = self.docker_manager.unpause_docker_services(yaml_content)  # type: ignore

            if unpause_result.get("success"):
                logging.info(f"Successfully unpaused service: {service_name}")
                self.progress_reporter.send_progress_update(
                    "completed", f"Successfully unpaused service {service_name}", 100
                )
            else:
                error_msg = f"Failed to unpause service {service_name}: {unpause_result.get('error', 'Unknown error')}"
                logging.error(error_msg)
                self.progress_reporter.send_progress_update("error", error_msg, 10)

        except Exception as e:
            error_msg = f"Error unpausing service {service_name}: {e}"
            logging.error(error_msg)
            self.progress_reporter.send_progress_update("error", error_msg, 10)

    def handle_restart_action(self, data: dict, service_name: str = ""):
        """
        Handle restart action for OTA operations.

        Parameters
        ----------
        data : dict
            The parsed message data containing restart details
        service_name : str
            The name of the service to restart
        """
        logging.info(f"Restarting service: {service_name}")
        self.progress_reporter.send_progress_update(
            "restarting", f"Restarting service {service_name}", 10
        )

        try:
            yaml_content = data.get("yaml_content")

            if not yaml_content:
                config_result = self.file_manager.load_latest_config(service_name)

                if config_result.get("success"):
                    yaml_content = config_result.get("yaml_content")
                    logging.info(
                        f"Loaded latest configuration from: {config_result.get('file_path')}"
                    )
                else:
                    error_msg = f"No YAML content provided and no stored configuration found for service {service_name}"
                    logging.error(error_msg)
                    self.progress_reporter.send_progress_update("error", error_msg, 10)
                    return

            restart_result = self.docker_manager.restart_docker_services(yaml_content)  # type: ignore

            if restart_result.get("success"):
                logging.info(f"Successfully restarted service: {service_name}")
                self.progress_reporter.send_progress_update(
                    "completed", f"Successfully restarted service {service_name}", 100
                )
            else:
                error_msg = f"Failed to restart service {service_name}: {restart_result.get('error', 'Unknown error')}"
                logging.error(error_msg)
                self.progress_reporter.send_progress_update("error", error_msg, 50)

        except Exception as e:
            error_msg = f"Error restarting service {service_name}: {e}"
            logging.error(error_msg)
            self.progress_reporter.send_progress_update("error", error_msg, 10)

    def apply_ota_update(
        self,
        service_name: str,
        yaml_content: dict,
        temp_yaml_path: str,
        tag: str,
    ):
        """
        Apply the OTA update based on the YAML content.

        Parameters
        ----------
        service_name : str
            The name of the service to be updated
        yaml_content : dict
            The parsed YAML content containing update instructions
        temp_yaml_path : str
            The temporary path of the downloaded YAML file
        tag : str
            The update tag/version
        """
        logging.info(f"Applying OTA update {tag} with content: {yaml_content}")

        self.progress_reporter.send_progress_update(
            "starting", f"Starting OTA update {tag}", 0
        )

        try:
            logging.info("Stopping current Docker services...")
            self.progress_reporter.send_progress_update(
                "stopping", "Stopping current Docker services", 10
            )
            stop_result = self.docker_manager.stop_docker_services(yaml_content)
            if not stop_result.get("success"):
                error_msg = f"Failed to stop Docker services: {stop_result.get('error', 'Unknown error')}"
                logging.error(error_msg)
                self.progress_reporter.send_progress_update("error", error_msg, 10)
                return False

            ecr_image = self.ecr_handler.check_image_privacy(yaml_content)
            if ecr_image and not self.ecr_handler.login_with_credentials(ecr_image):
                return False

            self.progress_reporter.send_progress_update(
                "storing", "Storing update files", 20
            )
            store_result = self.file_manager.store_update_files(
                service_name, tag, temp_yaml_path
            )

            if not store_result.get("success"):
                error_msg = store_result.get("error", "Unknown error storing files")
                logging.error(error_msg)
                self.progress_reporter.send_progress_update("error", error_msg, 20)
                return False

            logging.info("Starting updated Docker services...")
            start_result = self.docker_manager.start_docker_services(yaml_content)
            if not start_result.get("success"):
                error_msg = f"Failed to start Docker services: {start_result.get('error', 'Unknown error')}"
                logging.error(error_msg)
                self.progress_reporter.send_progress_update("error", error_msg, 80)
                return False

            logging.info(f"Successfully applied OTA update {tag}")
            self.progress_reporter.send_progress_update(
                "completed", f"Successfully applied OTA update {tag}", 100
            )
            return True

        except Exception as e:
            error_msg = f"Unexpected error during OTA update: {e}"
            logging.error(error_msg)
            self.progress_reporter.send_progress_update("error", error_msg, 0)
            return False

    def _extract_tag_from_yaml(self, yaml_content: dict) -> str:
        """
        Extract tag from the image name in yaml_content.
        """
        try:
            services = yaml_content.get("services", {})
            service = next(iter(services.values()), {})
            image = service.get("image", "")
            if ":" in image:
                return image.split(":")[-1]
            return "latest"
        except Exception:
            return "latest"
