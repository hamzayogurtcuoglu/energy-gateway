/**
 * Interactive official matter.js controller for the end-to-end demo.
 *
 * It commissions the local Matter node (device.js), then gives you a prompt to
 * switch it on/off in real time. Each command triggers the device's handler,
 * which calls the Go gateway, which writes the EEBUS production limit to the
 * inverter. The live on/off state is logged whenever it changes.
 *
 * Commands: on | off | toggle | status | help | quit
 */
import { createInterface } from "node:readline";
import { Environment, Logger, LogLevel } from "@matter/main";
import { GeneralCommissioning, OnOff } from "@matter/main/clusters";
import { ManualPairingCodeCodec } from "@matter/main/types";
import { CommissioningController } from "@project-chip/matter.js";

// Keep the prompt readable: only show warnings/errors from matter.js internals.
Logger.level = LogLevel.WARN;

const PAIRING_CODE = process.env.PAIRING_CODE ?? "34970112332";

const environment = Environment.default;
const decoded = ManualPairingCodeCodec.decode(PAIRING_CODE);

const controller = new CommissioningController({
    environment: { environment, id: "eebus-controller" },
    autoConnect: false,
    adminFabricLabel: "matter.js Controller",
});
await controller.start();

let nodeId;
if (!controller.isCommissioned()) {
    console.log("Commissioning the Matter node ...");
    nodeId = await controller.commissionNode({
        commissioning: {
            regulatoryLocation: GeneralCommissioning.RegulatoryLocationType.IndoorOutdoor,
            regulatoryCountryCode: "XX",
        },
        discovery: {
            identifierData: { shortDiscriminator: decoded.shortDiscriminator },
            discoveryCapabilities: { ble: false },
        },
        passcode: decoded.passcode,
    });
    console.log(`Commissioned, nodeId=${nodeId}`);
} else {
    nodeId = controller.getCommissionedNodes()[0];
    console.log(`Already commissioned, nodeId=${nodeId}`);
}

const node = await controller.getNode(nodeId);

// endpoint 1 = inverter (EEBUS), endpoint 2 = modbus.
const deviceNames = ["inverter", "modbus"];

// Log the live On/Off state of each device whenever it reports a change.
node.events.attributeChanged.on(({ path: { endpointId, attributeName }, value }) => {
    if (attributeName === "onOff") {
        const name = deviceNames[endpointId - 1] ?? `endpoint ${endpointId}`;
        console.log(`[state] ${name} is now ${value ? "ON" : "OFF"}`);
    }
});

if (!node.isConnected) {
    node.connect();
}
if (!node.initialized) {
    await node.events.initialized;
}

const devices = node.getDevices();

// Build a name -> OnOff client map. Endpoints are added in order on the device
// side: endpoint 1 = inverter, endpoint 2 = modbus.
const switches = {};
devices.forEach((device, index) => {
    const onOff = device.getClusterClient(OnOff.Complete);
    const name = deviceNames[index];
    if (onOff && name) {
        switches[name] = onOff;
    }
});
if (Object.keys(switches).length === 0) {
    throw new Error("no On/Off devices found");
}

async function printStates() {
    for (const [name, onOff] of Object.entries(switches)) {
        console.log(`  ${name}: ${(await onOff.getOnOffAttribute()) ? "ON" : "OFF"}`);
    }
}

const targets = Object.keys(switches).join(", ");
const help = `commands: <device> on|off|toggle  |  status  |  quit    (devices: ${targets})`;

console.log("initial state:");
await printStates();
console.log(help);

const rl = createInterface({ input: process.stdin, output: process.stdout, prompt: "matter> " });
rl.prompt();

rl.on("line", async line => {
    const parts = line.trim().toLowerCase().split(/\s+/).filter(Boolean);
    try {
        if (parts.length === 0) {
            // nothing typed
        } else if (["quit", "exit", "q"].includes(parts[0])) {
            rl.close();
            return;
        } else if (["help", "?"].includes(parts[0])) {
            console.log(help);
        } else if (["status", "s"].includes(parts[0])) {
            await printStates();
        } else if (parts.length === 2 && switches[parts[0]]) {
            const [name, action] = parts;
            const onOff = switches[name];
            if (action === "on") {
                await onOff.on();
                console.log(`[${name}] sent ON`);
            } else if (action === "off") {
                await onOff.off();
                console.log(`[${name}] sent OFF`);
            } else if (action === "toggle" || action === "t") {
                await onOff.toggle();
                console.log(`[${name}] sent TOGGLE`);
            } else {
                console.log(`unknown action "${action}"  (${help})`);
            }
        } else {
            console.log(`unknown command  (${help})`);
        }
    } catch (error) {
        console.error(`command failed: ${error.message}`);
    }
    rl.prompt();
});

rl.on("SIGINT", () => rl.close());

rl.on("close", async () => {
    console.log("closing controller ...");
    try {
        await controller.close();
    } catch {
        // ignore shutdown errors
    }
    process.exit(0);
});
