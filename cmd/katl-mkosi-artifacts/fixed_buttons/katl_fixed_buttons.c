// SPDX-License-Identifier: GPL-2.0
#include <linux/acpi.h>
#include <linux/module.h>
#include <linux/platform_device.h>

static struct platform_device *buttons[2];

static void unregister_buttons(void)
{
	unsigned int i;

	for (i = 0; i < ARRAY_SIZE(buttons); i++)
		if (buttons[i])
			platform_device_unregister(buttons[i]);
}

static int __init fixed_buttons_init(void)
{
	const char *ids[] = { "LNXPWRBN", "LNXSLPBN" };
	unsigned int i;

	/* Fixed buttons are enumerated before userspace and have no _CRS handle. */
	for (i = 0; i < ARRAY_SIZE(ids); i++) {
		struct platform_device_info info = {};
		struct platform_device *button;
		struct acpi_device *adev;

		adev = acpi_dev_get_first_match_dev(ids[i], NULL, -1);
		if (!adev)
			continue;
		if (adev->physical_node_count) {
			acpi_dev_put(adev);
			continue;
		}

		info.name = dev_name(&adev->dev);
		info.id = PLATFORM_DEVID_NONE;
		info.fwnode = acpi_fwnode_handle(adev);
		button = platform_device_register_full(&info);
		acpi_dev_put(adev);
		if (IS_ERR(button)) {
			unregister_buttons();
			return PTR_ERR(button);
		}
		buttons[i] = button;
	}
	return 0;
}

module_init(fixed_buttons_init);
module_exit(unregister_buttons);
MODULE_DESCRIPTION("Platform companions for ACPI fixed buttons on Linux 6.18.57-59");
MODULE_LICENSE("GPL");
