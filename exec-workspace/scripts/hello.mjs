// 执行器演示脚本：由 configs/config.yaml 的 executors.commands.hello_demo 档位调用。
const flag = process.argv.slice(2).filter((arg) => arg.startsWith('--day='));
console.log(`hello from godelayq executor ${flag.join(' ')}`);
