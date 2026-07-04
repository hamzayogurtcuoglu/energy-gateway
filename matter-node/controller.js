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

// Log the live On/Off state whenever the device reports a change. The device
// echoes its state back after every command, so this reflects the real switch
// state in real time.
node.events.attributeChanged.on(({ path: { attributeName }, value }) => {
    if (attributeName === "onOff") {
        console.log(`[state] switch is now ${value ? "ON" : "OFF"}`);
    }
});

if (!node.isConnected) {
    node.connect();
}
if (!node.initialized) {
    await node.events.initialized;
}

const devices = node.getDevices();
const onOff = devices[0].getClusterClient(OnOff.Complete);
if (onOff === undefined) {
    throw new Error("device has no OnOff cluster");
}

console.log(`initial state: ${(await onOff.getOnOffAttribute()) ? "ON" : "OFF"}`);

const help = "commands: on | off | toggle | status | help | quit";
const rl = createInterface({ input: process.stdin, output: process.stdout, prompt: "matter> " });
console.log(help);
rl.prompt();

rl.on("line", async line => {
    const cmd = line.trim().toLowerCase();
    try {
        switch (cmd) {
            case "on":
                await onOff.on();
                console.log("sent ON  -> inverter should curtail to the limit");
                break;
            case "off":
                await onOff.off();
                console.log("sent OFF -> inverter back to normal");
                break;
            case "toggle":
            case "t":
                await onOff.toggle();
                console.log("sent TOGGLE");
                break;
            case "status":
            case "s":
                console.log(`current state: ${(await onOff.getOnOffAttribute()) ? "ON" : "OFF"}`);
                break;
            case "help":
            case "?":
                console.log(help);
                break;
            case "quit":
            case "exit":
            case "q":
                rl.close();
                return;
            case "":
                break;
            default:
                console.log(`unknown command: "${cmd}"  (${help})`);
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
