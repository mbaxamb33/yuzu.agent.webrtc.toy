#!/usr/bin/env python3
"""Patch Python gRPC stubs to use relative imports."""
import os
import re
import sys

def patch_grpc_imports(directory: str, names: list[str]) -> None:
    for name in names:
        path = os.path.join(directory, f'{name}_pb2_grpc.py')
        try:
            content = open(path, 'r', encoding='utf-8').read()
        except FileNotFoundError:
            continue
        # Replace absolute import with relative import
        pattern = rf"(^|\n)import {name}_pb2 as .*"
        replacement = f"\nfrom . import {name}_pb2 as {name.replace('-', '_')}__pb2"
        new_content = re.sub(pattern, replacement, content)
        if new_content != content:
            open(path, 'w', encoding='utf-8').write(new_content)
            print(f'patched {path}')

if __name__ == '__main__':
    directory = sys.argv[1] if len(sys.argv) > 1 else 'gateway'
    names = ['stt', 'gateway_control', 'llm', 'tts']
    patch_grpc_imports(directory, names)
