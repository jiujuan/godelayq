<?php
// 执行器演示脚本：由 configs/config.yaml 的 executors.commands.php_hello 档位调用。
// 命令行由档位的 args_render 拼出：php <本文件> --day=<yesterday|today> --fail=<0|1>。

// 走 $_SERVER['argv'] 而不是 $argv：php.ini 里 register_argc_argv=Off 时 $argv 不存在，
// 而 $_SERVER['argv'] 仍然填好（本机实测），getopt() 在那种配置下会返回空表。
$argList = $_SERVER['argv'] ?? [];

$flags = [];
foreach (array_slice($argList, 1) as $arg) {
    if (str_starts_with($arg, '--') && str_contains($arg, '=')) {
        [$name, $value] = explode('=', substr($arg, 2), 2);
        $flags[$name] = $value;
    }
}

// getenv() 在 CLI 下返回全部环境变量；执行器只透传 executors.env_allow 里的键名。
$env = getenv();
$env = is_array($env) ? $env : [];

echo "hello from godelayq executor\n";
echo 'runtime=php ' . PHP_VERSION . "\n";
echo 'cwd=' . getcwd() . "\n";
echo 'day=' . ($flags['day'] ?? '<missing>') . "\n";
echo 'argv=' . implode(' ', array_slice($argList, 1)) . "\n";
echo 'env_count=' . count($env) . "\n";
foreach (['PATH', 'LANG', 'LC_ALL', 'TZ', 'HOME'] as $name) {
    echo 'env ' . $name . '=' . (array_key_exists($name, $env) ? 'set' : 'unset') . "\n";
}

if (($flags['fail'] ?? '') === '1') {
    fwrite(STDERR, "failing on purpose: exit 75, which retry_on_exit allows\n");
    exit(75);
}
exit(0);
